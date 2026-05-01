package serialcmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go-serial-cli/internal/diag"
	"go.bug.st/serial"
)

type StreamOptions struct {
	Port      string
	Baud      int
	Input     io.Reader
	Output    io.Writer
	TeePath   string
	CachePath string
}

type SerialPort interface {
	io.ReadWriteCloser
}

type AskOptions struct {
	Port      string
	Baud      int
	Data      string
	Timeout   time.Duration
	MaxLines  int
	Output    io.Writer
	CachePath string
	OpenPort  func(port string, baud int) (SerialPort, error)
}

type TCPBridgeOptions struct {
	ListenAddress string
	Port          string
	Baud          int
	CachePath     string
	AcceptOne     bool
	Stop          <-chan struct{}
	OpenPort      func(port string, baud int) (SerialPort, error)
	OnListening   func(address string)
}

type SessionServerOptions struct {
	ControlAddress string
	Port           string
	Baud           int
	CachePath      string
	Stop           <-chan struct{}
	OpenPort       func(port string, baud int) (SerialPort, error)
}

type ShareBridgeOptions struct {
	PhysicalPort   string
	HubPorts       []string
	Baud           int
	CachePath      string
	ControlAddress string
	TCPAddress     string
	Stop           <-chan struct{}
	OpenPort       func(port string, baud int) (SerialPort, error)
}

func Ports() ([]string, error) {
	ports, err := serial.GetPortsList()
	if err != nil {
		return nil, err
	}
	sort.Strings(ports)
	return ports, nil
}

func Send(port string, baud int, data string) error {
	if port == "" {
		return errors.New("port is required")
	}
	if baud <= 0 {
		return errors.New("baud must be positive")
	}

	p, err := serial.Open(port, &serial.Mode{BaudRate: baud})
	if err != nil {
		return diag.SerialOpenError(port, err)
	}
	defer p.Close()

	payload, err := parsePayload(data)
	if err != nil {
		return err
	}
	n, err := p.Write(payload)
	if err != nil {
		return fmt.Errorf("write serial port %s: %w", port, err)
	}
	if n != len(payload) {
		return fmt.Errorf("write serial port %s: short write %d/%d", port, n, len(payload))
	}
	return err
}

func Ask(opts AskOptions) error {
	if opts.Port == "" {
		return errors.New("port is required")
	}
	if opts.Baud <= 0 {
		return errors.New("baud must be positive")
	}
	if opts.Timeout <= 0 {
		return errors.New("ask timeout must be positive")
	}
	if opts.MaxLines < 0 {
		return errors.New("ask line limit must not be negative")
	}
	if opts.Output == nil {
		opts.Output = io.Discard
	}
	openPort := opts.OpenPort
	if openPort == nil {
		openPort = openSerialPort
	}

	p, err := openPort(opts.Port, opts.Baud)
	if err != nil {
		return diag.SerialOpenError(opts.Port, err)
	}
	defer p.Close()

	output, closeOutput, err := streamOutput(StreamOptions{Output: opts.Output, CachePath: opts.CachePath})
	if err != nil {
		return err
	}
	defer closeOutput()

	payload, err := parsePayload(opts.Data)
	if err != nil {
		return err
	}
	if err := writePayloadToPort(payload, p, opts.Port); err != nil {
		return err
	}
	return readAskResponse(p, output, opts.Port, opts.Timeout, opts.MaxLines)
}

func readAskResponse(port SerialPort, output io.Writer, portName string, timeout time.Duration, maxLines int) error {
	type readResult struct {
		data []byte
		n    int
		err  error
	}
	results := make(chan readResult, 1)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	response := make([]byte, 0, 4096)

	for {
		go func() {
			buf := make([]byte, 4096)
			n, err := port.Read(buf)
			results <- readResult{data: buf[:n], n: n, err: err}
		}()

		select {
		case <-timer.C:
			_ = port.Close()
			<-results
			_, err := output.Write(lastLines(response, maxLines))
			return err
		case result := <-results:
			if result.n > 0 {
				response = append(response, result.data...)
			}
			if result.err != nil {
				if errors.Is(result.err, io.EOF) {
					_, writeErr := output.Write(lastLines(response, maxLines))
					return writeErr
				}
				return fmt.Errorf("read serial port %s: %w", portName, result.err)
			}
		}
	}
}

func lastLines(data []byte, maxLines int) []byte {
	if maxLines <= 0 {
		return data
	}
	lines := 0
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] != '\n' {
			continue
		}
		lines++
		if lines > maxLines {
			return data[i+1:]
		}
	}
	return data
}

func Stream(opts StreamOptions) error {
	if opts.Port == "" {
		return errors.New("port is required")
	}
	if opts.Baud <= 0 {
		return errors.New("baud must be positive")
	}
	if opts.Output == nil {
		opts.Output = io.Discard
	}

	p, err := serial.Open(opts.Port, &serial.Mode{BaudRate: opts.Baud})
	if err != nil {
		return diag.SerialOpenError(opts.Port, err)
	}
	defer p.Close()

	output, closeOutput, err := streamOutput(opts)
	if err != nil {
		return err
	}
	defer closeOutput()

	errCh := make(chan error, 2)
	if opts.Input != nil {
		go func() {
			errCh <- copyInputToPort(opts.Input, p, opts.Port)
		}()
	}
	go func() {
		errCh <- copyPortToOutput(p, output, opts.Port)
	}()

	return <-errCh
}

func BridgeTCP(opts TCPBridgeOptions) error {
	if opts.ListenAddress == "" {
		return errors.New("listen address is required")
	}
	if opts.Port == "" {
		return errors.New("port is required")
	}
	if opts.Baud <= 0 {
		return errors.New("baud must be positive")
	}
	openPort := opts.OpenPort
	if openPort == nil {
		openPort = openSerialPort
	}

	port, err := openPort(opts.Port, opts.Baud)
	if err != nil {
		return diag.SerialOpenError(opts.Port, err)
	}
	defer port.Close()

	output, closeOutput, err := streamOutput(StreamOptions{Output: io.Discard, CachePath: opts.CachePath})
	if err != nil {
		return err
	}
	defer closeOutput()

	listener, err := net.Listen("tcp", opts.ListenAddress)
	if err != nil {
		return err
	}
	defer listener.Close()
	if opts.OnListening != nil {
		opts.OnListening(listener.Addr().String())
	}

	server := newSessionServer(port, output)
	defer server.closeClients()

	errCh := make(chan error, 3)
	go func() {
		errCh <- server.copyPortToClients(opts.Port)
	}()
	if opts.AcceptOne {
		go func() {
			errCh <- server.acceptOneClient(listener, opts.Port)
		}()
	} else {
		go func() {
			errCh <- server.acceptClients(listener, opts.Port)
		}()
	}
	if opts.Stop != nil {
		go func() {
			<-opts.Stop
			_ = listener.Close()
			_ = port.Close()
			server.closeClients()
		}()
	}

	err = <-errCh
	if opts.Stop != nil {
		select {
		case <-opts.Stop:
			return nil
		default:
		}
	}
	return normalizeCopyError(err)
}

func RunSessionServer(opts SessionServerOptions) error {
	if opts.ControlAddress == "" {
		return errors.New("control address is required")
	}
	if opts.Port == "" {
		return errors.New("port is required")
	}
	if opts.Baud <= 0 {
		return errors.New("baud must be positive")
	}
	openPort := opts.OpenPort
	if openPort == nil {
		openPort = openSerialPort
	}

	port, err := openPort(opts.Port, opts.Baud)
	if err != nil {
		return diag.SerialOpenError(opts.Port, err)
	}
	defer port.Close()

	output, closeOutput, err := streamOutput(StreamOptions{Output: io.Discard, CachePath: opts.CachePath})
	if err != nil {
		return err
	}
	defer closeOutput()

	listener, err := net.Listen("tcp", opts.ControlAddress)
	if err != nil {
		return err
	}
	defer listener.Close()

	server := newSessionServer(port, output)
	defer server.closeClients()

	errCh := make(chan error, 2)
	go func() {
		errCh <- server.copyPortToClients(opts.Port)
	}()
	go func() {
		errCh <- server.acceptClients(listener, opts.Port)
	}()
	if opts.Stop != nil {
		go func() {
			<-opts.Stop
			_ = listener.Close()
			_ = port.Close()
			server.closeClients()
		}()
	}

	err = <-errCh
	if opts.Stop != nil {
		select {
		case <-opts.Stop:
			return nil
		default:
		}
	}
	return normalizeCopyError(err)
}

func ShareBridge(opts ShareBridgeOptions) error {
	if opts.PhysicalPort == "" {
		return errors.New("physical port is required")
	}
	if len(opts.HubPorts) == 0 {
		return errors.New("hub ports are required")
	}
	if opts.Baud <= 0 {
		return errors.New("baud must be positive")
	}
	openPort := opts.OpenPort
	if openPort == nil {
		openPort = openSerialPort
	}

	output, closeOutput, err := streamOutput(StreamOptions{Output: io.Discard, CachePath: opts.CachePath})
	if err != nil {
		return err
	}
	defer closeOutput()

	endpoints := make([]shareEndpoint, 0, len(opts.HubPorts))
	defer closeShareEndpoints(endpoints)
	for _, hubPort := range opts.HubPorts {
		opened, err := openPort(hubPort, opts.Baud)
		if err != nil {
			return diag.SerialOpenError(hubPort, err)
		}
		endpoints = append(endpoints, shareEndpoint{name: hubPort, port: opened, writes: make(chan []byte, 256)})
	}

	bridge := newShareBridge(opts.PhysicalPort, opts.Baud, openPort, endpoints, output)
	bridge.startHubWriters()
	defer bridge.closeClients()

	var listeners []net.Listener
	if opts.ControlAddress != "" {
		listener, err := net.Listen("tcp", opts.ControlAddress)
		if err != nil {
			return err
		}
		listeners = append(listeners, listener)
		defer listener.Close()
	}
	if opts.TCPAddress != "" && opts.TCPAddress != opts.ControlAddress {
		listener, err := net.Listen("tcp", opts.TCPAddress)
		if err != nil {
			return err
		}
		listeners = append(listeners, listener)
		defer listener.Close()
	}

	errCh := make(chan error, len(endpoints)+len(listeners)+1)
	for i := range endpoints {
		endpoint := endpoints[i]
		go func() {
			errCh <- bridge.copyEndpointToOthers(endpoint)
		}()
	}
	for _, listener := range listeners {
		listener := listener
		go func() {
			errCh <- bridge.acceptControlClients(listener)
		}()
	}
	if opts.Stop != nil {
		go func() {
			<-opts.Stop
			for _, listener := range listeners {
				_ = listener.Close()
			}
			bridge.close()
		}()
	}

	err = <-errCh
	if opts.Stop != nil {
		select {
		case <-opts.Stop:
			return nil
		default:
		}
	}
	return normalizeCopyError(err)
}

func SendToSession(address string, data string) error {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	payload, err := parsePayload(data)
	if err != nil {
		return err
	}
	n, err := conn.Write(payload)
	if err != nil {
		return err
	}
	if n != len(payload) {
		return fmt.Errorf("write session control: short write %d/%d", n, len(payload))
	}
	return nil
}

func StreamSession(address string, input io.Reader, output io.Writer) error {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if output == nil {
		output = io.Discard
	}
	errCh := make(chan error, 2)
	if input != nil {
		go func() {
			errCh <- copyInputToPort(input, conn, address)
		}()
	}
	go func() {
		_, err := io.Copy(output, conn)
		errCh <- normalizeCopyError(err)
	}()
	return <-errCh
}

type sessionServer struct {
	port    SerialPort
	output  io.Writer
	mu      sync.Mutex
	writeMu sync.Mutex
	clients map[net.Conn]struct{}
}

type shareEndpoint struct {
	name   string
	port   SerialPort
	writes chan []byte
}

type shareBridge struct {
	physicalPort string
	baud         int
	openPort     func(port string, baud int) (SerialPort, error)
	endpoints    []shareEndpoint
	output       io.Writer
	mu           sync.Mutex
	writeMu      sync.Mutex
	clients      map[net.Conn]struct{}
	asyncWG      sync.WaitGroup
}

func newSessionServer(port SerialPort, output io.Writer) *sessionServer {
	return &sessionServer{
		port:    port,
		output:  output,
		clients: map[net.Conn]struct{}{},
	}
}

func newShareBridge(physicalPort string, baud int, openPort func(port string, baud int) (SerialPort, error), endpoints []shareEndpoint, output io.Writer) *shareBridge {
	return &shareBridge{
		physicalPort: physicalPort,
		baud:         baud,
		openPort:     openPort,
		endpoints:    endpoints,
		output:       output,
		clients:      map[net.Conn]struct{}{},
	}
}

func closeShareEndpoints(endpoints []shareEndpoint) {
	for _, endpoint := range endpoints {
		_ = endpoint.port.Close()
	}
}

func (b *shareBridge) close() {
	for _, endpoint := range b.endpoints {
		close(endpoint.writes)
	}
	closeShareEndpoints(b.endpoints)
	b.closeClients()
	b.asyncWG.Wait()
}

func (b *shareBridge) startHubWriters() {
	for i := range b.endpoints {
		endpoint := b.endpoints[i]
		b.asyncWG.Add(1)
		go func() {
			defer b.asyncWG.Done()
			for data := range endpoint.writes {
				_, _ = endpoint.port.Write(data)
			}
		}()
	}
}

func (b *shareBridge) acceptControlClients(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return normalizeCopyError(err)
		}
		b.addClient(conn)
		go func() {
			_ = b.copyClientToEndpoints(conn)
		}()
	}
}

func (b *shareBridge) addClient(conn net.Conn) {
	b.mu.Lock()
	b.clients[conn] = struct{}{}
	b.mu.Unlock()
}

func (b *shareBridge) removeClient(conn net.Conn) {
	b.mu.Lock()
	delete(b.clients, conn)
	b.mu.Unlock()
	_ = conn.Close()
}

func (b *shareBridge) closeClients() {
	b.mu.Lock()
	clients := make([]net.Conn, 0, len(b.clients))
	for conn := range b.clients {
		clients = append(clients, conn)
	}
	b.clients = map[net.Conn]struct{}{}
	b.mu.Unlock()
	for _, conn := range clients {
		_ = conn.Close()
	}
}

func (b *shareBridge) copyClientToEndpoints(conn net.Conn) error {
	defer b.removeClient(conn)
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			if writeErr := b.writePhysicalTransaction(data); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			return normalizeCopyError(err)
		}
	}
}

func (b *shareBridge) copyEndpointToOthers(source shareEndpoint) error {
	buf := make([]byte, 4096)
	for {
		n, err := source.port.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			if writeErr := b.routeEndpointData(source, data); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("read serial port %s: closed", source.name)
			}
			return fmt.Errorf("read serial port %s: %w", source.name, err)
		}
	}
}

func (b *shareBridge) routeEndpointData(source shareEndpoint, data []byte) error {
	return b.writePhysicalTransaction(data)
}

func (b *shareBridge) writePhysicalTransaction(data []byte) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	port, err := b.openPort(b.physicalPort, b.baud)
	if err != nil {
		return diag.SerialOpenError(b.physicalPort, err)
	}
	defer port.Close()
	if _, err := port.Write(data); err != nil {
		return fmt.Errorf("write serial port %s: %w", b.physicalPort, err)
	}
	return b.readPhysicalTransactionResponse(port)
}

func (b *shareBridge) readPhysicalTransactionResponse(port SerialPort) error {
	buf := make([]byte, 4096)
	for {
		n, err := readSerialWithIdleTimeout(port, buf, 150*time.Millisecond)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			if _, writeErr := b.output.Write(data); writeErr != nil {
				return writeErr
			}
			b.writeToHubEndpointsAsync(data)
			b.broadcast(data)
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read serial port %s: %w", b.physicalPort, err)
		}
		return nil
	}
}

func readSerialWithIdleTimeout(port SerialPort, buf []byte, idle time.Duration) (int, error) {
	if timeoutPort, ok := port.(interface{ SetReadTimeout(time.Duration) error }); ok {
		if err := timeoutPort.SetReadTimeout(idle); err != nil {
			return 0, err
		}
		return port.Read(buf)
	}
	type readResult struct {
		n   int
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		n, err := port.Read(buf)
		ch <- readResult{n: n, err: err}
	}()
	select {
	case result := <-ch:
		return result.n, result.err
	case <-time.After(idle):
		return 0, nil
	}
}

func (b *shareBridge) writeToHubEndpoints(data []byte) error {
	for _, endpoint := range b.endpoints {
		chunk := append([]byte(nil), data...)
		select {
		case endpoint.writes <- chunk:
		default:
		}
	}
	return nil
}

func (b *shareBridge) writeToHubEndpointsAsync(data []byte) {
	_ = b.writeToHubEndpoints(data)
}

func (b *shareBridge) broadcast(data []byte) {
	b.mu.Lock()
	clients := make([]net.Conn, 0, len(b.clients))
	for conn := range b.clients {
		clients = append(clients, conn)
	}
	b.mu.Unlock()
	for _, conn := range clients {
		if _, err := conn.Write(data); err != nil {
			b.removeClient(conn)
		}
	}
}

func (s *sessionServer) acceptClients(listener net.Listener, portName string) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return normalizeCopyError(err)
		}
		s.addClient(conn)
		go func() {
			_ = s.copyClientToPort(conn, portName)
		}()
	}
}

func (s *sessionServer) acceptOneClient(listener net.Listener, portName string) error {
	conn, err := listener.Accept()
	if err != nil {
		return normalizeCopyError(err)
	}
	s.addClient(conn)
	return s.copyClientToPort(conn, portName)
}

func (s *sessionServer) addClient(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[conn] = struct{}{}
}

func (s *sessionServer) removeClient(conn net.Conn) {
	s.mu.Lock()
	delete(s.clients, conn)
	s.mu.Unlock()
	_ = conn.Close()
}

func (s *sessionServer) closeClients() {
	s.mu.Lock()
	clients := make([]net.Conn, 0, len(s.clients))
	for conn := range s.clients {
		clients = append(clients, conn)
	}
	s.clients = map[net.Conn]struct{}{}
	s.mu.Unlock()
	for _, conn := range clients {
		_ = conn.Close()
	}
}

func (s *sessionServer) copyClientToPort(conn net.Conn, portName string) error {
	defer s.removeClient(conn)
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			s.writeMu.Lock()
			_, writeErr := s.port.Write(buf[:n])
			s.writeMu.Unlock()
			if writeErr != nil {
				return fmt.Errorf("write serial port %s: %w", portName, writeErr)
			}
		}
		if err != nil {
			return normalizeCopyError(err)
		}
	}
}

func (s *sessionServer) copyPortToClients(portName string) error {
	buf := make([]byte, 4096)
	for {
		n, err := s.port.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			if _, writeErr := s.output.Write(data); writeErr != nil {
				return writeErr
			}
			s.broadcast(data)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read serial port %s: %w", portName, err)
		}
	}
}

func (s *sessionServer) broadcast(data []byte) {
	s.mu.Lock()
	clients := make([]net.Conn, 0, len(s.clients))
	for conn := range s.clients {
		clients = append(clients, conn)
	}
	s.mu.Unlock()
	for _, conn := range clients {
		if _, err := conn.Write(data); err != nil {
			s.removeClient(conn)
		}
	}
}

func openSerialPort(portName string, baud int) (SerialPort, error) {
	return serial.Open(portName, &serial.Mode{BaudRate: baud})
}

func bridgeTCPConn(conn net.Conn, opts TCPBridgeOptions, openPort func(string, int) (SerialPort, error)) error {
	defer conn.Close()
	port, err := openPort(opts.Port, opts.Baud)
	if err != nil {
		return err
	}
	defer port.Close()

	output, closeOutput, err := streamOutput(StreamOptions{Output: conn, CachePath: opts.CachePath})
	if err != nil {
		return err
	}
	defer closeOutput()

	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(port, conn)
		_ = port.Close()
		errCh <- normalizeCopyError(err)
	}()
	go func() {
		_, err := io.Copy(output, port)
		_ = conn.Close()
		errCh <- normalizeCopyError(err)
	}()
	return <-errCh
}

func normalizeCopyError(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func streamOutput(opts StreamOptions) (io.Writer, func(), error) {
	writers := []io.Writer{opts.Output}
	var closers []io.Closer
	if opts.TeePath != "" {
		f, err := openAppendFile(opts.TeePath)
		if err != nil {
			return nil, func() {}, err
		}
		writers = append(writers, f)
		closers = append(closers, f)
	}
	if opts.CachePath != "" {
		f, err := openAppendFile(opts.CachePath)
		if err != nil {
			for _, closer := range closers {
				_ = closer.Close()
			}
			return nil, func() {}, err
		}
		writers = append(writers, f)
		closers = append(closers, f)
	}
	return io.MultiWriter(writers...), func() {
		for _, closer := range closers {
			_ = closer.Close()
		}
	}, nil
}

func openAppendFile(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

func copyInputToPort(input io.Reader, port io.Writer, portName string) error {
	reader := bufio.NewReader(input)
	var line []byte
	for {
		ch, err := reader.ReadByte()
		if err != nil {
			if len(line) > 0 {
				if writeErr := writeInputLineToPort(line, false, port, portName); writeErr != nil {
					return writeErr
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if isImmediateControlByte(ch) && len(line) == 0 {
			if err := writePayloadToPort([]byte{ch}, port, portName); err != nil {
				return err
			}
			continue
		}
		if ch == '\n' {
			if err := writeInputLineToPort(line, true, port, portName); err != nil {
				return err
			}
			line = line[:0]
			continue
		}
		line = append(line, ch)
	}
}

func writeInputLineToPort(line []byte, trimCR bool, port io.Writer, portName string) error {
	if trimCR && len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	payload, err := inputLinePayload(string(line))
	if err != nil {
		return err
	}
	return writePayloadToPort(payload, port, portName)
}

func writePayloadToPort(payload []byte, port io.Writer, portName string) error {
	n, err := port.Write(payload)
	if err != nil {
		return fmt.Errorf("write serial port %s: %w", portName, err)
	}
	if n != len(payload) {
		return fmt.Errorf("write serial port %s: short write %d/%d", portName, n, len(payload))
	}
	return nil
}

func isImmediateControlByte(ch byte) bool {
	return ch < 0x20 && ch != '\r' && ch != '\n' && ch != '\t'
}

func inputLinePayload(line string) ([]byte, error) {
	payload, err := parsePayload(line)
	if err != nil {
		return nil, err
	}
	if strings.Contains(line, `\r`) || strings.Contains(line, `\n`) || hasLineEnding(payload) {
		return payload, nil
	}
	return append(payload, '\r', '\n'), nil
}

func copyPortToOutput(port io.Reader, output io.Writer, portName string) error {
	buf := make([]byte, 4096)
	for {
		n, err := port.Read(buf)
		if n > 0 {
			if _, writeErr := output.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read serial port %s: %w", portName, err)
		}
	}
}

func hasLineEnding(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	last := value[len(value)-1]
	return last == '\r' || last == '\n'
}

func unescape(value string) string {
	payload, err := parsePayload(value)
	if err != nil {
		return value
	}
	return string(payload)
}

func parsePayload(value string) ([]byte, error) {
	payload := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			payload = append(payload, value[i])
			continue
		}
		if i+1 >= len(value) {
			payload = append(payload, value[i])
			continue
		}

		i++
		switch value[i] {
		case 'r':
			payload = append(payload, '\r')
		case 'n':
			payload = append(payload, '\n')
		case 't':
			payload = append(payload, '\t')
		case 'x':
			if i+2 >= len(value) {
				return nil, fmt.Errorf("invalid hex escape at byte %d: want \\xNN", i-1)
			}
			hi, ok := hexValue(value[i+1])
			if !ok {
				return nil, fmt.Errorf("invalid hex escape at byte %d: want \\xNN", i-1)
			}
			lo, ok := hexValue(value[i+2])
			if !ok {
				return nil, fmt.Errorf("invalid hex escape at byte %d: want \\xNN", i-1)
			}
			payload = append(payload, hi<<4|lo)
			i += 2
		case 'c':
			if i+1 >= len(value) {
				return nil, fmt.Errorf("invalid control escape at byte %d: want \\cA through \\cZ", i-1)
			}
			ch := value[i+1]
			if ch >= 'a' && ch <= 'z' {
				ch -= 'a' - 'A'
			}
			if ch < 'A' || ch > 'Z' {
				return nil, fmt.Errorf("invalid control escape at byte %d: want \\cA through \\cZ", i-1)
			}
			payload = append(payload, ch-'A'+1)
			i++
		default:
			payload = append(payload, '\\', value[i])
		}
	}
	return payload, nil
}

func hexValue(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}
