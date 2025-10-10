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
	RTTHistogram []HistogramBucket
	P95RTT       time.Duration
	P99RTT       time.Duration
}

type HistogramBucket struct {
	LowerBound time.Duration
	UpperBound time.Duration
	Count      int64
	Percentage float64
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
	
	// Optimize concurrent workers based on system and test size
	if concurrent <= 0 {
		concurrent = runtime.NumCPU()
	}
	// Limit concurrency to prevent overwhelming the server
	maxConcurrent := 50
	if concurrent > maxConcurrent {
		concurrent = maxConcurrent
	}
	lt.concurrent = concurrent

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
	fmt.Printf("Concurrent workers: %d\n", lt.concurrent)
	fmt.Printf("========================================")

	totalTests := len(payloadSizes) * len(protocols) * testPerSize
	var completedTests int64
	
	// Pre-allocate results slice to avoid frequent reallocations
	lt.results = make([]LatencyResult, 0, totalTests)

	// create worker pool with rate limiting
	taskChan := make(chan func(), concurrent*2) // Buffer to prevent blocking
	var wg sync.WaitGroup
	
	// start workers
	for i := 0; i < lt.concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskChan {
				task()
				completed := atomic.AddInt64(&completedTests, 1)
				if completed%100 == 0 || completed == int64(totalTests) {
					fmt.Printf("\rProgress: %d/%d (%.2f%%)", completed, totalTests, float64(completed)/float64(totalTests)*100)
				}
			}
		}()
	}

	for _, protocol := range protocols {
		for _, size := range payloadSizes {
			for i := 0; i < testPerSize; i++ {
				proto := protocol
				payloadSize := size
				taskChan <- func() {
					var result LatencyResult
					if proto == "tcp" {
						result = lt.testTCPPooled(payloadSize)
					} else if proto == "http" {
						result = lt.testHTTPPooled(payloadSize)
					}
					lt.resultsMutex.Lock()
					lt.results = append(lt.results, result)
					lt.resultsMutex.Unlock()
				}
			}
		}
	}

	close(taskChan)
	wg.Wait()

	// Final progress update
	fmt.Printf("\rProgress: %d/%d (100.00%%)", totalTests, totalTests)
	fmt.Printf("\nLatency tests completed.\n")
	
	// Force garbage collection to free up memory
	runtime.GC()
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
			
			// Also show protocol breakdown for this payload size
			protocolBreakdown := make(map[string][]LatencyResult)
			for _, result := range results {
				protocolBreakdown[result.Protocol] = append(protocolBreakdown[result.Protocol], result)
			}
			
			if len(protocolBreakdown) > 1 {
				fmt.Printf("  Protocol breakdown:\n")
				for protocol, protocolResults := range protocolBreakdown {
					protocolStats := calculateStatistics(protocol, size, protocolResults)
					fmt.Printf("    %s (%d tests): RTT Avg: %v, One-Way Avg: %v\n", 
						strings.ToUpper(protocol), protocolStats.Count, protocolStats.AvgRTT, protocolStats.AvgOneWay)
				}
				fmt.Printf("\n")
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

	// Calculate percentiles
	p95Index := int(float64(len(rttValues)) * 0.95)
	if p95Index >= len(rttValues) {
		p95Index = len(rttValues) - 1
	}
	stats.P95RTT = rttValues[p95Index]

	p99Index := int(float64(len(rttValues)) * 0.99)
	if p99Index >= len(rttValues) {
		p99Index = len(rttValues) - 1
	}
	stats.P99RTT = rttValues[p99Index]

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

	// Calculate histogram
	stats.RTTHistogram = calculateHistogram(rttValues)

	return stats
}

func calculateHistogram(sortedValues []time.Duration) []HistogramBucket {
	if len(sortedValues) == 0 {
		return nil
	}

	minVal := sortedValues[0]
	maxVal := sortedValues[len(sortedValues)-1]
	
	// Create 10 buckets
	numBuckets := 10
	buckets := make([]HistogramBucket, numBuckets)
	
	// Calculate bucket size
	bucketSize := time.Duration(float64(maxVal-minVal) / float64(numBuckets))
	if bucketSize == 0 {
		bucketSize = time.Microsecond // Minimum bucket size
	}

	// Initialize buckets
	for i := 0; i < numBuckets; i++ {
		buckets[i].LowerBound = minVal + time.Duration(i)*bucketSize
		if i == numBuckets-1 {
			buckets[i].UpperBound = maxVal
		} else {
			buckets[i].UpperBound = minVal + time.Duration(i+1)*bucketSize
		}
	}

	// Count values in each bucket
	valueIndex := 0
	for i := 0; i < numBuckets && valueIndex < len(sortedValues); i++ {
		for valueIndex < len(sortedValues) && sortedValues[valueIndex] <= buckets[i].UpperBound {
			buckets[i].Count++
			valueIndex++
		}
		buckets[i].Percentage = float64(buckets[i].Count) / float64(len(sortedValues)) * 100
	}

	return buckets
}

func printProtocolStats(stats Statistics) {
	fmt.Printf("%s Protocol:\n", strings.ToUpper(stats.Protocol))
	fmt.Printf("  Tests: %d\n", stats.Count)
	fmt.Printf("  RTT - Avg: %v, Min: %v, Max: %v, Median: %v, StdDev: %v\n",
		stats.AvgRTT, stats.MinRTT, stats.MaxRTT, stats.MedianRTT, stats.StdDevRTT)
	fmt.Printf("  RTT - P95: %v, P99: %v\n", stats.P95RTT, stats.P99RTT)
	fmt.Printf("  One-Way - Avg: %v, Min: %v, Max: %v\n",
		stats.AvgOneWay, stats.MinOneWay, stats.MaxOneWay)
	
	// Print histogram
	if len(stats.RTTHistogram) > 0 {
		fmt.Printf("  RTT Histogram:\n")
		printHistogram(stats.RTTHistogram)
	}
	fmt.Printf("\n")
}

func printSizeStats(stats Statistics) {
	fmt.Printf("Payload Size %d bytes:\n", stats.PayloadSize)
	fmt.Printf("  Tests: %d\n", stats.Count)
	fmt.Printf("  RTT - Avg: %v, Min: %v, Max: %v, Median: %v, StdDev: %v\n",
		stats.AvgRTT, stats.MinRTT, stats.MaxRTT, stats.MedianRTT, stats.StdDevRTT)
	fmt.Printf("  RTT - P95: %v, P99: %v\n", stats.P95RTT, stats.P99RTT)
	fmt.Printf("  One-Way - Avg: %v, Min: %v, Max: %v\n",
		stats.AvgOneWay, stats.MinOneWay, stats.MaxOneWay)
	
	// Print histogram
	if len(stats.RTTHistogram) > 0 {
		fmt.Printf("  RTT Histogram:\n")
		printHistogram(stats.RTTHistogram)
	}
	fmt.Printf("\n")
}

func printHistogram(histogram []HistogramBucket) {
	// Find the maximum count for scaling the visual bars
	maxCount := int64(0)
	for _, bucket := range histogram {
		if bucket.Count > maxCount {
			maxCount = bucket.Count
		}
	}
	
	// Print each bucket
	for _, bucket := range histogram {
		if bucket.Count == 0 {
			continue // Skip empty buckets
		}
		
		// Create visual bar (max 50 characters)
		barLength := int(float64(bucket.Count) / float64(maxCount) * 50)
		bar := strings.Repeat("█", barLength)
		if barLength == 0 && bucket.Count > 0 {
			bar = "▌" // Show at least something for non-zero counts
		}
		
		fmt.Printf("    %8v - %8v: %6d (%5.1f%%) %s\n", 
			bucket.LowerBound, bucket.UpperBound, bucket.Count, bucket.Percentage, bar)
	}
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
