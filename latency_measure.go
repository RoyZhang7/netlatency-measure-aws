package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Global histogram configuration
var (
	globalHistogramBuckets int
	globalHistogramWidth   time.Duration
	globalHistogramEnabled bool
)

type TimeSyncPacket struct {
	ServerSendTime    int64
	ServerReceiveTime int64
	ClientSendTime    int64
}

// Represents the result of a single latency packet
type LatencyResult struct {
	Protocol      string
	PayloadSize   int
	RTT           time.Duration
	OneWayLatency time.Duration

	// the difference between the client and server clock
	// client send time t1
	// server receive time t2
	// server send time t3
	// client receive time t4
	// clock offset = t2-t1 + t3-t4 / 2
	ClockOffset time.Duration
	Success     bool
	Error       string
}

type Histogram struct {
	BucketRanges []time.Duration // Upper bounds for each bucket
	BucketCounts []int64         // Count of values in each bucket
	BucketWidth  time.Duration   // Width of each bucket (for fixed-width histograms)
	MinValue     time.Duration   // Minimum observed value
	MaxValue     time.Duration   // Maximum observed value
	TotalCount   int64           // Total number of values
}

type Statistics struct {
	Protocol     string
	PayloadSize  int
	Count        int64
	SuccessCount int64
	FailureCount int64
	AvgRTT       time.Duration
	MinRTT       time.Duration
	MaxRTT       time.Duration
	MedianRTT    time.Duration
	StdDevRTT    time.Duration
	AvgOneWay    time.Duration
	MinOneWay    time.Duration
	MaxOneWay    time.Duration
	Percentiles  map[float64]time.Duration // P50, P75, P90, P95, P99, P99.9, P99.99
	RTTHistogram *Histogram                // Histogram for RTT values
	OneWayHistogram *Histogram             // Histogram for one-way latency values
}

type PercentileData struct {
	Percentile float64
	Value      time.Duration
}

// TCP server data structure and implementation
type TCPServer struct {
	port        int
	listener    net.Listener
	clockOffset int64
}

func NewTCPServer(port int) *TCPServer {
	return &TCPServer{port: port}
}

func (s *TCPServer) Start() error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return err
	}
	s.listener = listener
	fmt.Printf("TCP server listening on port %d\n", s.port)

	go s.acceptConnections()
	return nil
}

func (s *TCPServer) acceptConnections() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConnection(conn)
	}
}

func (s *TCPServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	const maxPayloadSize = 10 * 1024 * 1024 // 10MB limit
	
	// Reuse buffers for this connection
	sizeBuf := make([]byte, 4)
	timestampBuf := make([]byte, 8)
	var payload []byte
	var response []byte
	
	requestCount := 0

	for {
		// read payload size
		if _, err := io.ReadFull(conn, sizeBuf); err != nil {
			return
		}
		payloadSize := binary.BigEndian.Uint32(sizeBuf)

		// validate payload size to prevent memory exhaustion
		if payloadSize > maxPayloadSize {
			fmt.Printf("Rejecting oversized payload: %d bytes (max: %d)\n", payloadSize, maxPayloadSize)
			return
		}

		// read client timestamp
		if _, err := io.ReadFull(conn, timestampBuf); err != nil {
			return
		}
		clientSendstamp := int64(binary.BigEndian.Uint64(timestampBuf))

		// record server receive time
		serverRecvTime := time.Now().UnixNano()

		// reuse or allocate payload buffer
		if cap(payload) < int(payloadSize) {
			payload = make([]byte, payloadSize)
		} else {
			payload = payload[:payloadSize]
		}
		
		if _, err := io.ReadFull(conn, payload); err != nil {
			return
		}

		// reuse or allocate response buffer
		responseSize := 4 + 8 + 8 + 8 + int(payloadSize)
		if cap(response) < responseSize {
			response = make([]byte, responseSize)
		} else {
			response = response[:responseSize]
		}
		
		binary.BigEndian.PutUint32(response[0:4], payloadSize)
		binary.BigEndian.PutUint64(response[4:12], uint64(clientSendstamp))
		binary.BigEndian.PutUint64(response[12:20], uint64(serverRecvTime))

		// record server send time
		serverSendTime := time.Now().UnixNano()
		binary.BigEndian.PutUint64(response[20:28], uint64(serverSendTime))
		copy(response[28:], payload)

		if _, err := conn.Write(response); err != nil {
			return
		}
		
		requestCount++
		// Periodically trigger GC to prevent memory buildup
		if requestCount%1000 == 0 {
			runtime.GC()
		}
	}
}

func (s *TCPServer) Stop() {
	s.listener.Close()
}

// HTTP server data structure and implementation
type HTTPServer struct {
	port   int
	server *http.Server
}

func NewHTTPServer(port int) *HTTPServer {
	return &HTTPServer{port: port}
}

func (s *HTTPServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", s.handleEcho)

	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}

	fmt.Printf("HTTP server listening on port %d\n", s.port)
	go func() {
		if err := s.server.ListenAndServe(); err != http.ErrServerClosed {
			fmt.Printf("HTTP server error: %v\n", err)
		}
	}()

	return nil
}

// Global buffer pool for HTTP server
var httpBufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 0, 64*1024) // 64KB initial capacity
	},
}

func (s *HTTPServer) handleEcho(w http.ResponseWriter, r *http.Request) {
	const maxPayloadSize = 10 * 1024 * 1024 // 10MB limit

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check content length to prevent memory exhaustion
	if r.ContentLength > maxPayloadSize {
		http.Error(w, fmt.Sprintf("Payload too large: %d bytes (max: %d)", r.ContentLength, maxPayloadSize), http.StatusRequestEntityTooLarge)
		return
	}

	// Read client timestamp from header
	clientSendTime := time.Now().UnixNano()
	if ts := r.Header.Get("X-Client-Timestamp"); ts != "" {
		fmt.Sscanf(ts, "%d", &clientSendTime)
	}

	serverRecvTime := time.Now().UnixNano()

	// Use buffer pool for reading body
	buf := httpBufferPool.Get().([]byte)
	defer httpBufferPool.Put(buf[:0])
	
	// Ensure buffer has enough capacity
	if int64(cap(buf)) < r.ContentLength {
		buf = make([]byte, 0, r.ContentLength)
	}
	
	// Read body with size limit using buffer
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadSize))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	serverSendTime := time.Now().UnixNano()

	// Send back with timestamps in headers
	w.Header().Set("X-Client-Timestamp", fmt.Sprintf("%d", clientSendTime))
	w.Header().Set("X-Server-Recv-Time", fmt.Sprintf("%d", serverRecvTime))
	w.Header().Set("X-Server-Send-Time", fmt.Sprintf("%d", serverSendTime))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(body)
}

func (s *HTTPServer) Stop() {
	if s.server != nil {
		s.server.Close()
	}
}

// Connection pool for TCP connections
type TCPConnectionPool struct {
	connections chan net.Conn
	host        string
	port        int
	timeout     time.Duration
	maxSize     int
}

func NewTCPConnectionPool(host string, port int, timeout time.Duration, maxSize int) *TCPConnectionPool {
	return &TCPConnectionPool{
		connections: make(chan net.Conn, maxSize),
		host:        host,
		port:        port,
		timeout:     timeout,
		maxSize:     maxSize,
	}
}

func (pool *TCPConnectionPool) Get() (net.Conn, error) {
	select {
	case conn := <-pool.connections:
		// Test if connection is still valid
		conn.SetReadDeadline(time.Now().Add(time.Millisecond))
		one := make([]byte, 1)
		if _, err := conn.Read(one); err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// Connection is good, reset deadline
				conn.SetReadDeadline(time.Time{})
				return conn, nil
			}
			// Connection is bad, close it and create new one
			conn.Close()
		}
	default:
		// No connection available, create new one
	}
	
	return net.DialTimeout("tcp", fmt.Sprintf("%s:%d", pool.host, pool.port), pool.timeout)
}

func (pool *TCPConnectionPool) Put(conn net.Conn) {
	if conn == nil {
		return
	}
	
	select {
	case pool.connections <- conn:
		// Successfully returned to pool
	default:
		// Pool is full, close the connection
		conn.Close()
	}
}

func (pool *TCPConnectionPool) Close() {
	close(pool.connections)
	for conn := range pool.connections {
		conn.Close()
	}
}

// Buffer pool for reusing byte slices
type BufferPool struct {
	pool sync.Pool
}

func NewBufferPool() *BufferPool {
	return &BufferPool{
		pool: sync.Pool{
			New: func() interface{} {
				return make([]byte, 0, 64*1024) // 64KB initial capacity
			},
		},
	}
}

func (bp *BufferPool) Get(size int) []byte {
	buf := bp.pool.Get().([]byte)
	if cap(buf) < size {
		return make([]byte, size)
	}
	return buf[:size]
}

func (bp *BufferPool) Put(buf []byte) {
	if cap(buf) <= 1024*1024 { // Only pool buffers up to 1MB
		bp.pool.Put(buf[:0])
	}
}

// Latency tester data structure and implementation
type LatencyTester struct {
	host              string
	tcpPort           int
	httpPort          int
	protocols         []string
	payloadSizes      []int
	testPerSize       int
	concurrent        int
	timeout           time.Duration
	results           []LatencyResult
	resultsMutex      sync.Mutex
	clockOffset       time.Duration
	clockOffsetSynced bool
	tcpPool           *TCPConnectionPool
	bufferPool        *BufferPool
	httpClient        *http.Client
	rateLimit         float64
	maxWorkers        int
	requestedWorkers  int
	activeWorkers     int64
	workerStats       []WorkerStats
	statsMutex        sync.RWMutex
	scalingMutex      sync.Mutex
	taskQueue         chan func() LatencyResult
	workerScaler      *WorkerScaler
	completedTests    int64
}

// WorkerStats tracks per-worker performance metrics
type WorkerStats struct {
	WorkerID        int
	PacketsSent     int64
	PacketsSuccess  int64
	PacketsFailed   int64
	TotalLatency    time.Duration
	LastPacketTime  time.Time
	IsActive        bool
	StartTime       time.Time
}

// WorkerScaler manages dynamic worker scaling based on queue depth and load
type WorkerScaler struct {
	tester           *LatencyTester
	stopChan         chan struct{}
	scalingInterval  time.Duration
	queueThreshold   int
	scaleUpDelay     time.Duration
	scaleDownDelay   time.Duration
	lastScaleAction  time.Time
}

func NewWorkerScaler(tester *LatencyTester) *WorkerScaler {
	return &WorkerScaler{
		tester:          tester,
		stopChan:        make(chan struct{}),
		scalingInterval: 2 * time.Second,  // Check every 2 seconds
		queueThreshold:  10,               // Scale up if queue > 10 tasks
		scaleUpDelay:    5 * time.Second,  // Wait 5s between scale-ups
		scaleDownDelay:  10 * time.Second, // Wait 10s between scale-downs
	}
}

// Start begins the worker scaling monitoring
func (ws *WorkerScaler) Start() {
	go ws.scalingLoop()
}

// Stop stops the worker scaling monitoring
func (ws *WorkerScaler) Stop() {
	close(ws.stopChan)
}

// scalingLoop monitors queue depth and scales workers accordingly
func (ws *WorkerScaler) scalingLoop() {
	ticker := time.NewTicker(ws.scalingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ws.stopChan:
			return
		case <-ticker.C:
			ws.checkAndScale()
		}
	}
}

// checkAndScale evaluates current load and scales workers if needed
func (ws *WorkerScaler) checkAndScale() {
	ws.tester.scalingMutex.Lock()
	defer ws.tester.scalingMutex.Unlock()

	currentWorkers := int(atomic.LoadInt64(&ws.tester.activeWorkers))
	queueDepth := len(ws.tester.taskQueue)
	
	now := time.Now()
	
	// Scale up if queue is deep and we haven't scaled recently
	if queueDepth > ws.queueThreshold && 
	   currentWorkers < ws.tester.maxWorkers && 
	   now.Sub(ws.lastScaleAction) > ws.scaleUpDelay {
		
		newWorkers := min(currentWorkers+2, ws.tester.maxWorkers) // Scale up by 2
		ws.scaleUp(newWorkers - currentWorkers)
		ws.lastScaleAction = now
		fmt.Printf("\n[SCALING] Queue depth: %d, scaling UP to %d workers\n", queueDepth, newWorkers)
		
	} else if queueDepth == 0 && 
			  currentWorkers > ws.tester.requestedWorkers && 
			  now.Sub(ws.lastScaleAction) > ws.scaleDownDelay {
		
		newWorkers := max(currentWorkers-1, ws.tester.requestedWorkers) // Scale down by 1
		ws.scaleDown(currentWorkers - newWorkers)
		ws.lastScaleAction = now
		fmt.Printf("\n[SCALING] Queue empty, scaling DOWN to %d workers\n", newWorkers)
	}
}

// scaleUp adds new workers
func (ws *WorkerScaler) scaleUp(count int) {
	for i := 0; i < count; i++ {
		ws.startNewWorker()
	}
}

// scaleDown removes workers (they will exit when queue is empty)
func (ws *WorkerScaler) scaleDown(count int) {
	// Workers will naturally exit when queue is empty and they exceed requested count
	// This is handled in the worker loop
}

// startNewWorker creates and starts a new worker goroutine
func (ws *WorkerScaler) startNewWorker() {
	workerID := len(ws.tester.workerStats)
	
	// Extend worker stats array
	ws.tester.statsMutex.Lock()
	ws.tester.workerStats = append(ws.tester.workerStats, WorkerStats{
		WorkerID:  workerID,
		IsActive:  true,
		StartTime: time.Now(),
	})
	ws.tester.statsMutex.Unlock()
	
	atomic.AddInt64(&ws.tester.activeWorkers, 1)
	
	go ws.tester.workerLoop(workerID)
}

// Helper functions
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func NewLatencyTester(host string, tcpPort int, httpPort int) *LatencyTester {
	// Create HTTP client with connection pooling
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	
	return &LatencyTester{
		host:       host,
		tcpPort:    tcpPort,
		httpPort:   httpPort,
		results:    make([]LatencyResult, 0),
		bufferPool: NewBufferPool(),
		httpClient: httpClient,
	}
}

func (lt *LatencyTester) RunTests(protocols []string, payloadSizes []int, testPerSize int, concurrent int, timeout time.Duration) {
	lt.protocols = protocols
	lt.payloadSizes = payloadSizes
	lt.testPerSize = testPerSize
	lt.timeout = timeout
	
	// Set up Kubernetes-style request/limit model
	if concurrent <= 0 {
		concurrent = runtime.NumCPU()
	}
	
	// Validate request vs limit
	if concurrent > lt.maxWorkers {
		fmt.Printf("Warning: Requested workers (%d) exceeds limit (%d). Using limit as request.\n", concurrent, lt.maxWorkers)
		concurrent = lt.maxWorkers
	}
	
	lt.requestedWorkers = concurrent  // Request (guaranteed minimum)
	lt.concurrent = concurrent        // For compatibility
	
	// Initialize task queue with buffer
	lt.taskQueue = make(chan func() LatencyResult, lt.maxWorkers*2)
	
	// Initialize worker stats for requested workers
	lt.workerStats = make([]WorkerStats, 0, lt.maxWorkers)
	for i := 0; i < lt.requestedWorkers; i++ {
		lt.workerStats = append(lt.workerStats, WorkerStats{
			WorkerID:  i,
			IsActive:  true,
			StartTime: time.Now(),
		})
	}
	
	// Initialize worker scaler
	lt.workerScaler = NewWorkerScaler(lt)

	// Initialize TCP connection pool if TCP protocol is used
	if contains(protocols, "tcp") {
		lt.tcpPool = NewTCPConnectionPool(lt.host, lt.tcpPort, timeout, concurrent*2)
		defer lt.tcpPool.Close()
	}

	// sync clock
	if err := lt.SyncClock(); err != nil {
		fmt.Printf("Warning: Continuing without syncing clock. (oneway latency will be inaccurate)\n ")
		return
	}

	fmt.Printf("========================================")
	fmt.Printf("\nStarting latency tests...")
	fmt.Printf("Target: %s:%d (TCP) %s:%d (HTTP)", lt.host, lt.tcpPort, lt.host, lt.httpPort)
	fmt.Printf("Payload sizes: %v bytes", lt.payloadSizes)
	fmt.Printf("Protocols: %v", lt.protocols)
	fmt.Printf("Workers: %d requested, %d limit", lt.requestedWorkers, lt.maxWorkers)
	if lt.rateLimit > 0 {
		fmt.Printf(" (Rate limited: %.1f pps)", lt.rateLimit)
	}
	fmt.Printf("\n========================================")

	totalTests := len(payloadSizes) * len(protocols) * testPerSize
	atomic.StoreInt64(&lt.completedTests, 0)
	
	// Pre-allocate results slice to avoid frequent reallocations
	lt.results = make([]LatencyResult, 0, totalTests)

	var wg sync.WaitGroup
	
	// Start initial requested workers
	atomic.StoreInt64(&lt.activeWorkers, int64(lt.requestedWorkers))
	for i := 0; i < lt.requestedWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			lt.workerLoop(workerID)
		}(i)
	}
	
	// Start worker scaler
	lt.workerScaler.Start()
	defer lt.workerScaler.Stop()

	// Queue all tasks
	go func() {
		for _, protocol := range protocols {
			for _, size := range payloadSizes {
				for i := 0; i < testPerSize; i++ {
					proto := protocol
					payloadSize := size
					task := func() LatencyResult {
						if proto == "tcp" {
							return lt.testTCPPooled(payloadSize)
						} else if proto == "http" {
							return lt.testHTTPPooled(payloadSize)
						}
						return LatencyResult{Protocol: proto, PayloadSize: payloadSize, Success: false, Error: "Unknown protocol"}
					}
					
					// Add task to queue (blocks if queue is full)
					lt.taskQueue <- task
				}
			}
		}
		close(lt.taskQueue)
	}()

	// Monitor progress
	go func() {
		startTime := time.Now()
		for {
			completed := atomic.LoadInt64(&lt.completedTests)
			if completed >= int64(totalTests) {
				break
			}
			
			if completed%100 == 0 && completed > 0 {
				elapsed := time.Since(startTime)
				rate := float64(completed) / elapsed.Seconds()
				activeWorkers := atomic.LoadInt64(&lt.activeWorkers)
				fmt.Printf("\rProgress: %d/%d (%.2f%%) - Rate: %.1f pps - Workers: %d", 
					completed, totalTests, float64(completed)/float64(totalTests)*100, rate, activeWorkers)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()

	wg.Wait()

	// Final progress update
	fmt.Printf("\rProgress: %d/%d (100.00%%)", totalTests, totalTests)
	fmt.Printf("\nLatency tests completed.\n")
	
	// Force garbage collection to free up memory
	runtime.GC()
	
	// Display worker statistics if enabled
	lt.printWorkerStats()
}

// printWorkerStats displays per-worker performance statistics
func (lt *LatencyTester) printWorkerStats() {
	if len(lt.workerStats) == 0 {
		return
	}
	
	fmt.Printf("\n========================================\n")
	fmt.Printf("WORKER PERFORMANCE STATISTICS\n")
	fmt.Printf("========================================\n")
	
	totalSent := int64(0)
	totalSuccess := int64(0)
	totalFailed := int64(0)
	
	lt.statsMutex.RLock()
	activeCount := 0
	for _, stats := range lt.workerStats {
		totalSent += stats.PacketsSent
		totalSuccess += stats.PacketsSuccess
		totalFailed += stats.PacketsFailed
		
		if stats.IsActive {
			activeCount++
		}
		
		successRate := float64(0)
		avgLatency := time.Duration(0)
		if stats.PacketsSent > 0 {
			successRate = float64(stats.PacketsSuccess) / float64(stats.PacketsSent) * 100
		}
		if stats.PacketsSuccess > 0 {
			avgLatency = stats.TotalLatency / time.Duration(stats.PacketsSuccess)
		}
		
		status := "INACTIVE"
		if stats.IsActive {
			status = "ACTIVE"
		}
		
		fmt.Printf("Worker %d [%s]: Sent: %d, Success: %d (%.1f%%), Failed: %d, Avg Latency: %v\n",
			stats.WorkerID, status, stats.PacketsSent, stats.PacketsSuccess, successRate, 
			stats.PacketsFailed, avgLatency)
	}
	lt.statsMutex.RUnlock()
	
	fmt.Printf("\nScaling Summary: %d active workers, %d total created\n", activeCount, len(lt.workerStats))
	
	overallSuccessRate := float64(0)
	if totalSent > 0 {
		overallSuccessRate = float64(totalSuccess) / float64(totalSent) * 100
	}
	
	fmt.Printf("\nOverall: Sent: %d, Success: %d (%.1f%%), Failed: %d\n",
		totalSent, totalSuccess, overallSuccessRate, totalFailed)
	fmt.Printf("========================================\n")
}

// workerLoop is the main loop for each worker goroutine
func (lt *LatencyTester) workerLoop(workerID int) {
	// Calculate rate limiting parameters for this worker
	var rateLimiter *time.Ticker
	var rateLimitChan <-chan time.Time
	if lt.rateLimit > 0 {
		// Distribute rate limit across all potential workers (maxWorkers)
		perWorkerRate := lt.rateLimit / float64(lt.maxWorkers)
		if perWorkerRate > 0 {
			interval := time.Duration(float64(time.Second) / perWorkerRate)
			rateLimiter = time.NewTicker(interval)
			rateLimitChan = rateLimiter.C
			defer rateLimiter.Stop()
		}
	}
	
	for {
		// Check if we should exit (scale down)
		currentWorkers := int(atomic.LoadInt64(&lt.activeWorkers))
		if currentWorkers > lt.requestedWorkers && len(lt.taskQueue) == 0 {
			// This worker can exit to scale down
			atomic.AddInt64(&lt.activeWorkers, -1)
			
			// Mark worker as inactive
			lt.statsMutex.Lock()
			if workerID < len(lt.workerStats) {
				lt.workerStats[workerID].IsActive = false
			}
			lt.statsMutex.Unlock()
			
			return
		}
		
		// Get next task from queue
		task, ok := <-lt.taskQueue
		if !ok {
			// Queue is closed, exit
			atomic.AddInt64(&lt.activeWorkers, -1)
			
			// Mark worker as inactive
			lt.statsMutex.Lock()
			if workerID < len(lt.workerStats) {
				lt.workerStats[workerID].IsActive = false
			}
			lt.statsMutex.Unlock()
			
			return
		}
		
		// Rate limiting (if enabled)
		if rateLimitChan != nil {
			<-rateLimitChan
		}
		
		// Execute the task
		result := task()
		
		// Update worker stats
		lt.statsMutex.Lock()
		if workerID < len(lt.workerStats) {
			lt.workerStats[workerID].PacketsSent++
			if result.Success {
				lt.workerStats[workerID].PacketsSuccess++
				lt.workerStats[workerID].TotalLatency += result.RTT
			} else {
				lt.workerStats[workerID].PacketsFailed++
			}
			lt.workerStats[workerID].LastPacketTime = time.Now()
		}
		lt.statsMutex.Unlock()
		
		// Store result
		lt.resultsMutex.Lock()
		lt.results = append(lt.results, result)
		lt.resultsMutex.Unlock()
		
		// Update progress counter
		atomic.AddInt64(&lt.completedTests, 1)
	}
}

// SyncClock implements NTP-like algorithm to synchronize clocks
func (lt *LatencyTester) SyncClock() error {
	const syncAttempts = 5
	offsets := make([]time.Duration, 0, syncAttempts)

	fmt.Printf("Synchronizing clocks with %d attempts...\n", syncAttempts)

	for i := 0; i < syncAttempts; i++ {
		offset, err := lt.measureClockOffset()
		if err != nil {
			fmt.Printf("Clock sync attempt %d failed: %v\n", i+1, err)
			continue
		}
		offsets = append(offsets, offset)
	}

	if len(offsets) == 0 {
		return fmt.Errorf("all clock synchronization attempts failed")
	}

	// Use median filtering to avoid outliers
	sort.Slice(offsets, func(i, j int) bool {
		return offsets[i] < offsets[j]
	})

	lt.clockOffset = offsets[len(offsets)/2]
	lt.clockOffsetSynced = true

	fmt.Printf("Clock synchronized. Offset: %v\n", lt.clockOffset)
	return nil
}

func (lt *LatencyTester) measureClockOffset() (time.Duration, error) {
	// Try TCP first, fallback to HTTP
	if contains(lt.protocols, "tcp") {
		return lt.measureClockOffsetTCP()
	} else if contains(lt.protocols, "http") {
		return lt.measureClockOffsetHTTP()
	}
	return 0, fmt.Errorf("no supported protocol for clock sync")
}

func (lt *LatencyTester) measureClockOffsetTCP() (time.Duration, error) {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", lt.host, lt.tcpPort), lt.timeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	t1 := time.Now().UnixNano() // Client send time

	// Send minimal payload for sync
	payload := make([]byte, 8)
	request := make([]byte, 4+8+len(payload))
	binary.BigEndian.PutUint32(request[0:4], uint32(len(payload)))
	binary.BigEndian.PutUint64(request[4:12], uint64(t1))
	copy(request[12:], payload)

	if _, err := conn.Write(request); err != nil {
		return 0, err
	}

	// Read response
	response := make([]byte, 4+8+8+8+len(payload))
	if _, err := io.ReadFull(conn, response); err != nil {
		return 0, err
	}

	t4 := time.Now().UnixNano() // Client receive time

	t2 := int64(binary.BigEndian.Uint64(response[12:20])) // Server receive time
	t3 := int64(binary.BigEndian.Uint64(response[20:28])) // Server send time

	// Clock offset = ((t2 - t1) + (t3 - t4)) / 2
	offset := time.Duration(((t2 - t1) + (t3 - t4)) / 2)
	return offset, nil
}

func (lt *LatencyTester) measureClockOffsetHTTP() (time.Duration, error) {
	client := &http.Client{Timeout: lt.timeout}

	t1 := time.Now().UnixNano()

	req, err := http.NewRequest("POST", fmt.Sprintf("http://%s:%d/echo", lt.host, lt.httpPort), bytes.NewReader(make([]byte, 8)))
	if err != nil {
		return 0, err
	}

	req.Header.Set("X-Client-Timestamp", fmt.Sprintf("%d", t1))
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	t4 := time.Now().UnixNano()

	t2, _ := strconv.ParseInt(resp.Header.Get("X-Server-Recv-Time"), 10, 64)
	t3, _ := strconv.ParseInt(resp.Header.Get("X-Server-Send-Time"), 10, 64)

	offset := time.Duration(((t2 - t1) + (t3 - t4)) / 2)
	return offset, nil
}

func (lt *LatencyTester) testTCP(payloadSize int) LatencyResult {
	result := LatencyResult{
		Protocol:    "tcp",
		PayloadSize: payloadSize,
		Success:     false,
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", lt.host, lt.tcpPort), lt.timeout)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer conn.Close()

	t1 := time.Now().UnixNano()

	// Prepare request
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	request := make([]byte, 4+8+payloadSize)
	binary.BigEndian.PutUint32(request[0:4], uint32(payloadSize))
	binary.BigEndian.PutUint64(request[4:12], uint64(t1))
	copy(request[12:], payload)

	if _, err := conn.Write(request); err != nil {
		result.Error = err.Error()
		return result
	}

	// Read response
	response := make([]byte, 4+8+8+8+payloadSize)
	if _, err := io.ReadFull(conn, response); err != nil {
		result.Error = err.Error()
		return result
	}

	t4 := time.Now().UnixNano()

	t2 := int64(binary.BigEndian.Uint64(response[12:20]))
	t3 := int64(binary.BigEndian.Uint64(response[20:28]))

	result.RTT = time.Duration(t4 - t1)
	result.ClockOffset = time.Duration(((t2 - t1) + (t3 - t4)) / 2)

	if lt.clockOffsetSynced {
		result.OneWayLatency = time.Duration(t2 - t1 - int64(lt.clockOffset))
	} else {
		result.OneWayLatency = time.Duration(t2 - t1 - int64(result.ClockOffset))
	}

	result.Success = true
	return result
}

func (lt *LatencyTester) testTCPPooled(payloadSize int) LatencyResult {
	result := LatencyResult{
		Protocol:    "tcp",
		PayloadSize: payloadSize,
		Success:     false,
	}

	conn, err := lt.tcpPool.Get()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	
	// Set timeout for this test
	conn.SetDeadline(time.Now().Add(lt.timeout))
	defer func() {
		conn.SetDeadline(time.Time{}) // Clear deadline
		lt.tcpPool.Put(conn)
	}()

	t1 := time.Now().UnixNano()

	// Use buffer pool for payload
	payload := lt.bufferPool.Get(payloadSize)
	defer lt.bufferPool.Put(payload)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	// Use buffer pool for request
	requestSize := 4 + 8 + payloadSize
	request := lt.bufferPool.Get(requestSize)
	defer lt.bufferPool.Put(request)
	
	binary.BigEndian.PutUint32(request[0:4], uint32(payloadSize))
	binary.BigEndian.PutUint64(request[4:12], uint64(t1))
	copy(request[12:], payload)

	if _, err := conn.Write(request); err != nil {
		result.Error = err.Error()
		return result
	}

	// Use buffer pool for response
	responseSize := 4 + 8 + 8 + 8 + payloadSize
	response := lt.bufferPool.Get(responseSize)
	defer lt.bufferPool.Put(response)
	
	if _, err := io.ReadFull(conn, response); err != nil {
		result.Error = err.Error()
		return result
	}

	t4 := time.Now().UnixNano()

	t2 := int64(binary.BigEndian.Uint64(response[12:20]))
	t3 := int64(binary.BigEndian.Uint64(response[20:28]))

	result.RTT = time.Duration(t4 - t1)
	result.ClockOffset = time.Duration(((t2 - t1) + (t3 - t4)) / 2)

	if lt.clockOffsetSynced {
		result.OneWayLatency = time.Duration(t2 - t1 - int64(lt.clockOffset))
	} else {
		result.OneWayLatency = time.Duration(t2 - t1 - int64(result.ClockOffset))
	}

	result.Success = true
	return result
}

func (lt *LatencyTester) testHTTP(payloadSize int) LatencyResult {
	result := LatencyResult{
		Protocol:    "http",
		PayloadSize: payloadSize,
		Success:     false,
	}

	client := &http.Client{Timeout: lt.timeout}

	t1 := time.Now().UnixNano()

	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("http://%s:%d/echo", lt.host, lt.httpPort), bytes.NewReader(payload))
	if err != nil {
		result.Error = err.Error()
		return result
	}

	req.Header.Set("X-Client-Timestamp", fmt.Sprintf("%d", t1))
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer resp.Body.Close()

	t4 := time.Now().UnixNano()

	t2, _ := strconv.ParseInt(resp.Header.Get("X-Server-Recv-Time"), 10, 64)
	t3, _ := strconv.ParseInt(resp.Header.Get("X-Server-Send-Time"), 10, 64)

	result.RTT = time.Duration(t4 - t1)
	result.ClockOffset = time.Duration(((t2 - t1) + (t3 - t4)) / 2)

	if lt.clockOffsetSynced {
		result.OneWayLatency = time.Duration(t2 - t1 - int64(lt.clockOffset))
	} else {
		result.OneWayLatency = time.Duration(t2 - t1 - int64(result.ClockOffset))
	}

	result.Success = true
	return result
}

func (lt *LatencyTester) testHTTPPooled(payloadSize int) LatencyResult {
	result := LatencyResult{
		Protocol:    "http",
		PayloadSize: payloadSize,
		Success:     false,
	}

	t1 := time.Now().UnixNano()

	// Use buffer pool for payload
	payload := lt.bufferPool.Get(payloadSize)
	defer lt.bufferPool.Put(payload)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("http://%s:%d/echo", lt.host, lt.httpPort), bytes.NewReader(payload))
	if err != nil {
		result.Error = err.Error()
		return result
	}

	req.Header.Set("X-Client-Timestamp", fmt.Sprintf("%d", t1))
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := lt.httpClient.Do(req)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer resp.Body.Close()

	t4 := time.Now().UnixNano()

	t2, _ := strconv.ParseInt(resp.Header.Get("X-Server-Recv-Time"), 10, 64)
	t3, _ := strconv.ParseInt(resp.Header.Get("X-Server-Send-Time"), 10, 64)

	result.RTT = time.Duration(t4 - t1)
	result.ClockOffset = time.Duration(((t2 - t1) + (t3 - t4)) / 2)

	if lt.clockOffsetSynced {
		result.OneWayLatency = time.Duration(t2 - t1 - int64(lt.clockOffset))
	} else {
		result.OneWayLatency = time.Duration(t2 - t1 - int64(result.ClockOffset))
	}

	result.Success = true
	return result
}

func (lt *LatencyTester) AnalyzeResults() {
	fmt.Printf("========================================\n")
	fmt.Printf("LATENCY TEST RESULTS\n")
	fmt.Printf("========================================\n")

	totalTests := len(lt.results)
	successfulTest := 0
	for _, r := range lt.results {
		if r.Success {
			successfulTest++
		}
	}

	fmt.Printf("SUMMARY:\n\n")
	fmt.Printf("Total tests: %d\n", totalTests)
	fmt.Printf("Successful: %d\n", successfulTest)
	fmt.Printf("Failure: %d\n", totalTests-successfulTest)
	fmt.Printf("Success Rate: %.1f%%\n", float64(successfulTest)/float64(totalTests)*100)
	if lt.clockOffsetSynced {
		fmt.Printf("Clock Offset: %v\n", lt.clockOffset)
	}
	fmt.Printf("\n========================================\n")

	// Group by protocol
	protocolStats := make(map[string][]LatencyResult)
	for _, result := range lt.results {
		if result.Success {
			protocolStats[result.Protocol] = append(protocolStats[result.Protocol], result)
		}
	}

	fmt.Printf("RESULTS BY PROTOCOL:\n\n")
	for protocol, results := range protocolStats {
		stats := calculateStatistics(protocol, 0, results)
		printProtocolStats(stats)
	}

	// Group by payload size
	sizeStats := make(map[int][]LatencyResult)
	for _, result := range lt.results {
		if result.Success {
			sizeStats[result.PayloadSize] = append(sizeStats[result.PayloadSize], result)
		}
	}

	fmt.Printf("\nRESULTS BY PAYLOAD SIZE:\n\n")
	for _, size := range lt.payloadSizes {
		if results, exists := sizeStats[size]; exists {
			stats := calculateStatistics("", size, results)
			printSizeStats(stats)
			
			// Show separate protocol histograms for this payload size
			protocolBreakdown := make(map[string][]LatencyResult)
			for _, result := range results {
				protocolBreakdown[result.Protocol] = append(protocolBreakdown[result.Protocol], result)
			}
			
			if len(protocolBreakdown) > 1 {
				fmt.Printf("  Protocol Breakdown:\n")
				for protocol, protocolResults := range protocolBreakdown {
					protocolStats := calculateStatistics(protocol, size, protocolResults)
					fmt.Printf("    %s Protocol (%d tests):\n", strings.ToUpper(protocol), protocolStats.Count)
					fmt.Printf("      RTT - Avg: %v, Min: %v, Max: %v, StdDev: %v\n", 
						protocolStats.AvgRTT, protocolStats.MinRTT, protocolStats.MaxRTT, protocolStats.StdDevRTT)
					fmt.Printf("      One-Way - Avg: %v, Min: %v, Max: %v\n",
						protocolStats.AvgOneWay, protocolStats.MinOneWay, protocolStats.MaxOneWay)
					
					if len(protocolStats.Percentiles) > 0 {
						fmt.Printf("      Latency Distribution:\n")
						printPercentiles(protocolStats.Percentiles)
					}
					
					// Print histograms for protocol breakdown
					if protocolStats.RTTHistogram != nil && protocolStats.RTTHistogram.TotalCount > 0 {
						fmt.Printf("      ")
						protocolStats.RTTHistogram.PrintHistogram("RTT Histogram", 25)
					}
					fmt.Printf("\n")
				}
			}
		}
	}
}

func calculateStatistics(protocol string, payloadSize int, results []LatencyResult) Statistics {
	if len(results) == 0 {
		return Statistics{}
	}

	stats := Statistics{
		Protocol:     protocol,
		PayloadSize:  payloadSize,
		Count:        int64(len(results)),
		SuccessCount: int64(len(results)),
		FailureCount: 0,
	}

	// Sort RTT values for median calculation
	rttValues := make([]time.Duration, len(results))
	oneWayValues := make([]time.Duration, len(results))

	var rttSum, oneWaySum time.Duration

	for i, result := range results {
		rttValues[i] = result.RTT
		oneWayValues[i] = result.OneWayLatency
		rttSum += result.RTT
		oneWaySum += result.OneWayLatency
	}

	sort.Slice(rttValues, func(i, j int) bool { return rttValues[i] < rttValues[j] })
	sort.Slice(oneWayValues, func(i, j int) bool { return oneWayValues[i] < oneWayValues[j] })

	// Calculate RTT statistics
	stats.AvgRTT = rttSum / time.Duration(len(results))
	stats.MinRTT = rttValues[0]
	stats.MaxRTT = rttValues[len(rttValues)-1]
	stats.MedianRTT = rttValues[len(rttValues)/2]

	// Calculate standard deviation for RTT
	var rttVarianceSum float64
	for _, rtt := range rttValues {
		diff := float64(rtt - stats.AvgRTT)
		rttVarianceSum += diff * diff
	}
	stats.StdDevRTT = time.Duration(math.Sqrt(rttVarianceSum / float64(len(results))))

	// Calculate One-Way statistics
	stats.AvgOneWay = oneWaySum / time.Duration(len(results))
	stats.MinOneWay = oneWayValues[0]
	stats.MaxOneWay = oneWayValues[len(oneWayValues)-1]

	// Calculate percentiles
	stats.Percentiles = calculatePercentiles(rttValues)

	// Create histograms based on global configuration
	if globalHistogramEnabled {
		if globalHistogramWidth > 0 {
			// Use fixed-width histograms
			stats.RTTHistogram = NewFixedWidthHistogram(rttValues, globalHistogramWidth)
			stats.OneWayHistogram = NewFixedWidthHistogram(oneWayValues, globalHistogramWidth)
		} else {
			// Use dynamic bucket sizing
			stats.RTTHistogram = NewHistogram(rttValues, globalHistogramBuckets)
			stats.OneWayHistogram = NewHistogram(oneWayValues, globalHistogramBuckets)
		}
	}

	return stats
}

func calculatePercentiles(sortedValues []time.Duration) map[float64]time.Duration {
	if len(sortedValues) == 0 {
		return nil
	}

	percentiles := map[float64]time.Duration{}
	targets := []float64{10.0, 30.0, 50.0, 75.0, 90.0, 95.0, 99.0, 99.9, 99.99}
	
	for _, p := range targets {
		index := int(float64(len(sortedValues)) * p / 100.0)
		if index >= len(sortedValues) {
			index = len(sortedValues) - 1
		}
		percentiles[p] = sortedValues[index]
	}
	
	return percentiles
}

func printProtocolStats(stats Statistics) {
	fmt.Printf("%s Protocol:\n", strings.ToUpper(stats.Protocol))
	fmt.Printf("  Tests: %d\n", stats.Count)
	fmt.Printf("  RTT - Avg: %v, Min: %v, Max: %v, Median: %v, StdDev: %v\n",
		stats.AvgRTT, stats.MinRTT, stats.MaxRTT, stats.MedianRTT, stats.StdDevRTT)
	fmt.Printf("  One-Way - Avg: %v, Min: %v, Max: %v\n",
		stats.AvgOneWay, stats.MinOneWay, stats.MaxOneWay)
	
	// Print percentile distribution
	if len(stats.Percentiles) > 0 {
		fmt.Printf("  Latency Distribution:\n")
		printPercentiles(stats.Percentiles)
	}
	
	// Print histograms
	if stats.RTTHistogram != nil && stats.RTTHistogram.TotalCount > 0 {
		stats.RTTHistogram.PrintHistogram("RTT Histogram", 30)
	}
	if stats.OneWayHistogram != nil && stats.OneWayHistogram.TotalCount > 0 {
		stats.OneWayHistogram.PrintHistogram("One-Way Latency Histogram", 30)
	}
	fmt.Printf("\n")
}

func printSizeStats(stats Statistics) {
	fmt.Printf("Payload Size %d bytes:\n", stats.PayloadSize)
	fmt.Printf("  Tests: %d\n", stats.Count)
	fmt.Printf("  RTT - Avg: %v, Min: %v, Max: %v, Median: %v, StdDev: %v\n",
		stats.AvgRTT, stats.MinRTT, stats.MaxRTT, stats.MedianRTT, stats.StdDevRTT)
	fmt.Printf("  One-Way - Avg: %v, Min: %v, Max: %v\n",
		stats.AvgOneWay, stats.MinOneWay, stats.MaxOneWay)
	
	// Print percentile distribution
	if len(stats.Percentiles) > 0 {
		fmt.Printf("  Latency Distribution:\n")
		printPercentiles(stats.Percentiles)
	}
	
	// Print histograms
	if stats.RTTHistogram != nil && stats.RTTHistogram.TotalCount > 0 {
		stats.RTTHistogram.PrintHistogram("RTT Histogram", 30)
	}
	if stats.OneWayHistogram != nil && stats.OneWayHistogram.TotalCount > 0 {
		stats.OneWayHistogram.PrintHistogram("One-Way Latency Histogram", 30)
	}
	fmt.Printf("\n")
}

func printPercentiles(percentiles map[float64]time.Duration) {
	// Define the order of percentiles to print (including P10 and P30)
	order := []float64{10.0, 30.0, 50.0, 75.0, 90.0, 95.0, 99.0, 99.9, 99.99}
	
	for _, p := range order {
		if value, exists := percentiles[p]; exists {
			if p == 10.0 {
				fmt.Printf("   10.000%%  %8v\n", value)
			} else if p == 30.0 {
				fmt.Printf("   30.000%%  %8v\n", value)
			} else if p == 50.0 {
				fmt.Printf("   50.000%%  %8v\n", value)
			} else if p == 75.0 {
				fmt.Printf("   75.000%%  %8v\n", value)
			} else if p == 90.0 {
				fmt.Printf("   90.000%%  %8v\n", value)
			} else if p == 95.0 {
				fmt.Printf("   95.000%%  %8v\n", value)
			} else if p == 99.0 {
				fmt.Printf("   99.000%%  %8v\n", value)
			} else if p == 99.9 {
				fmt.Printf("   99.900%%  %8v\n", value)
			} else if p == 99.99 {
				fmt.Printf("   99.990%%  %8v\n", value)
			}
		}
	}
}

// NewHistogram creates a new histogram with dynamic bucket sizing based on data range
func NewHistogram(values []time.Duration, bucketCount int) *Histogram {
	if len(values) == 0 {
		return &Histogram{}
	}

	// Sort values to find min/max
	sortedValues := make([]time.Duration, len(values))
	copy(sortedValues, values)
	sort.Slice(sortedValues, func(i, j int) bool { return sortedValues[i] < sortedValues[j] })

	minVal := sortedValues[0]
	maxVal := sortedValues[len(sortedValues)-1]

	// If all values are the same, create a single bucket
	if minVal == maxVal {
		return &Histogram{
			BucketRanges: []time.Duration{maxVal},
			BucketCounts: []int64{int64(len(values))},
			BucketWidth:  0,
			MinValue:     minVal,
			MaxValue:     maxVal,
			TotalCount:   int64(len(values)),
		}
	}

	// Calculate bucket width
	bucketWidth := (maxVal - minVal) / time.Duration(bucketCount)
	if bucketWidth == 0 {
		bucketWidth = time.Microsecond // Minimum bucket width
	}

	// Create bucket ranges
	bucketRanges := make([]time.Duration, bucketCount)
	bucketCounts := make([]int64, bucketCount)

	for i := 0; i < bucketCount; i++ {
		bucketRanges[i] = minVal + time.Duration(i+1)*bucketWidth
	}
	// Ensure the last bucket includes the maximum value
	bucketRanges[bucketCount-1] = maxVal

	// Count values in each bucket
	for _, value := range values {
		for i, upperBound := range bucketRanges {
			if value <= upperBound {
				bucketCounts[i]++
				break
			}
		}
	}

	return &Histogram{
		BucketRanges: bucketRanges,
		BucketCounts: bucketCounts,
		BucketWidth:  bucketWidth,
		MinValue:     minVal,
		MaxValue:     maxVal,
		TotalCount:   int64(len(values)),
	}
}

// NewFixedWidthHistogram creates a histogram with fixed bucket width
func NewFixedWidthHistogram(values []time.Duration, bucketWidth time.Duration) *Histogram {
	if len(values) == 0 {
		return &Histogram{}
	}

	// Sort values to find min/max
	sortedValues := make([]time.Duration, len(values))
	copy(sortedValues, values)
	sort.Slice(sortedValues, func(i, j int) bool { return sortedValues[i] < sortedValues[j] })

	minVal := sortedValues[0]
	maxVal := sortedValues[len(sortedValues)-1]

	// Calculate number of buckets needed
	bucketCount := int((maxVal-minVal)/bucketWidth) + 1

	// Create bucket ranges
	bucketRanges := make([]time.Duration, bucketCount)
	bucketCounts := make([]int64, bucketCount)

	for i := 0; i < bucketCount; i++ {
		bucketRanges[i] = minVal + time.Duration(i+1)*bucketWidth
	}
	// Ensure the last bucket includes the maximum value
	bucketRanges[bucketCount-1] = maxVal

	// Count values in each bucket
	for _, value := range values {
		for i, upperBound := range bucketRanges {
			if value <= upperBound {
				bucketCounts[i]++
				break
			}
		}
	}

	return &Histogram{
		BucketRanges: bucketRanges,
		BucketCounts: bucketCounts,
		BucketWidth:  bucketWidth,
		MinValue:     minVal,
		MaxValue:     maxVal,
		TotalCount:   int64(len(values)),
	}
}

// PrintHistogram prints a visual representation of the histogram
func (h *Histogram) PrintHistogram(title string, maxBarWidth int) {
	if h.TotalCount == 0 {
		fmt.Printf("  %s: No data\n", title)
		return
	}

	fmt.Printf("  %s:\n", title)

	// Find the maximum count for scaling
	maxCount := int64(0)
	for _, count := range h.BucketCounts {
		if count > maxCount {
			maxCount = count
		}
	}

	if maxCount == 0 {
		fmt.Printf("    No data in buckets\n")
		return
	}

	// Print each bucket
	lowerBound := h.MinValue
	for i, upperBound := range h.BucketRanges {
		count := h.BucketCounts[i]
		percentage := float64(count) / float64(h.TotalCount) * 100

		// Create bar representation
		barLength := int(float64(count) / float64(maxCount) * float64(maxBarWidth))
		bar := strings.Repeat("█", barLength)
		if barLength == 0 && count > 0 {
			bar = "▌" // Show something for very small counts
		}

		// Format the range
		if i == 0 {
			fmt.Printf("    [%8v - %8v]: %6d (%5.1f%%) %s\n", 
				lowerBound, upperBound, count, percentage, bar)
		} else {
			fmt.Printf("    (%8v - %8v]: %6d (%5.1f%%) %s\n", 
				lowerBound, upperBound, count, percentage, bar)
		}
		lowerBound = upperBound
	}
	fmt.Printf("    Total: %d samples, Range: %v - %v\n", 
		h.TotalCount, h.MinValue, h.MaxValue)
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func main() {
	mode := flag.String("mode", "", "Run as a client or server")
	//host := flag.String("host", "", "Host IP to bind server to or target for client")
	tcpPort := flag.Int("tcpPort", 0, "TCP Port to connect to")
	httpPort := flag.Int("httpPort", 0, "HTTP Port to connect to")

	// client options
	target := flag.String("target", "", "Target server IP to connect to (required for client mode)")
	protocolsFlag := flag.String("protocols", "tcp,http", "Protocols to test (comma separated, like tcp,http)")
	payloadSizesFlag := flag.String("payloadSizes", "", "Comma-separated list of payload sizes in bytes (e.g., 64,128,256,512,1024)")
	payloadMin := flag.Int("payloadMin", 64, "Minimum payload size to test in bytes (used if payloadSizes not specified)")
	payloadMax := flag.Int("payloadMax", 1024, "Maximum payload size to test in bytes (used if payloadSizes not specified)")
	testPerSize := flag.Int("testPerSize", 10, "Number of tests per payload size")
	concurrent := flag.Int("concurrent", 1, "Number of concurrent connections to test")
	timeout := flag.Int("timeout", 10, "Timeout in seconds")
	histogramBuckets := flag.Int("histogramBuckets", 10, "Number of histogram buckets (0 to disable histograms)")
	histogramWidth := flag.String("histogramWidth", "", "Fixed histogram bucket width (e.g., '5ms', '10ms'). If not set, uses dynamic buckets.")
	rateLimit := flag.Float64("rateLimit", 0, "Rate limit in packets per second (0 = no limit)")
	maxWorkers := flag.Int("maxWorkers", 50, "Maximum number of concurrent workers (default: 50)")

	// parse flags
	flag.Parse()

	if *mode == "" {
		fmt.Println("Mode is required")
		flag.Usage()
		return
	}

	if *mode == "client" {
		if *target == "" {
			fmt.Println("Target is required for client mode")
			flag.Usage()
			return
		}

		// Configure histogram settings
		globalHistogramBuckets = *histogramBuckets
		globalHistogramEnabled = *histogramBuckets > 0
		
		if *histogramWidth != "" {
			var err error
			globalHistogramWidth, err = time.ParseDuration(*histogramWidth)
			if err != nil {
				fmt.Printf("Invalid histogram width '%s': %v\n", *histogramWidth, err)
				return
			}
		}

		protocols := make([]string, 0)
		for _, p := range strings.Split(*protocolsFlag, ",") {
			protocols = append(protocols, strings.TrimSpace(p) )
		}

		// Parse payload sizes from command line or use defaults
		var payloadSizes []int
		if *payloadSizesFlag != "" {
			// Parse custom payload sizes
			sizeStrings := strings.Split(*payloadSizesFlag, ",")
			payloadSizes = make([]int, 0, len(sizeStrings))
			for _, sizeStr := range sizeStrings {
				sizeStr = strings.TrimSpace(sizeStr)
				size, err := strconv.Atoi(sizeStr)
				if err != nil {
					fmt.Printf("Invalid payload size '%s': %v\n", sizeStr, err)
					return
				}
				if size <= 0 {
					fmt.Printf("Payload size must be positive: %d\n", size)
					return
				}
				payloadSizes = append(payloadSizes, size)
			}
		} else if *payloadMin != 64 || *payloadMax != 1024 {
			// Generate sizes from min to max (doubling)
			payloadSizes = make([]int, 0)
			for size := *payloadMin; size <= *payloadMax; size *= 2 {
				payloadSizes = append(payloadSizes, size)
			}
			if payloadSizes[len(payloadSizes)-1] != *payloadMax {
				payloadSizes = append(payloadSizes, *payloadMax)
			}
		} else {
			// Use default sizes
			payloadSizes = []int{64, 128, 256, 512, 1024, 2048, 4096}
		}

		tester := NewLatencyTester(*target, *tcpPort, *httpPort)
		tester.rateLimit = *rateLimit
		tester.maxWorkers = *maxWorkers
		tester.RunTests(protocols, payloadSizes, *testPerSize, *concurrent, time.Duration(*timeout)*time.Second)
		tester.AnalyzeResults()
	} else if *mode == "server" {
		fmt.Println("Startting server...")

		tcpServer := NewTCPServer(*tcpPort)
		if err := tcpServer.Start(); err != nil {
			fmt.Printf("Failed to start TCP server: %v\n", err)
			return
		}

		httpServer := NewHTTPServer(*httpPort)
		if err := httpServer.Start(); err != nil {
			fmt.Printf("Failed to start HTTP server: %v\n", err)
			return
		}

		fmt.Println("Server is running. Press Ctrl+C to stop.")
		select {}
	}
}
