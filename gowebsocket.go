package gowebsocket

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sacOO7/go-logger"
)

type Empty struct {
}

var logger = logging.GetLogger(reflect.TypeOf(Empty{}).PkgPath()).SetLevel(logging.OFF)

func (socket Socket) EnableLogging() {
	logger.SetLevel(logging.TRACE)
}

func (socket Socket) GetLogger() logging.Logger {
	return logger
}

type Socket struct {
	Conn                *websocket.Conn
	WebsocketDialer     *websocket.Dialer
	Url                 string
	ConnectionOptions   ConnectionOptions
	ReconnectionOptions ReconnectionOptions
	RequestHeader       http.Header
	OnConnected         func(socket Socket)
	OnTextMessage       func(message string, socket Socket)
	OnBinaryMessage     func(data []byte, socket Socket)
	OnConnectError      func(err error, socket Socket)
	OnDisconnected      func(err error, socket Socket)
	OnPingReceived      func(data string, socket Socket)
	OnPongReceived      func(data string, socket Socket)
	IsConnected         bool
	Timeout             time.Duration
	sendMu              *sync.Mutex // Prevent "concurrent write to websocket connection"
	receiveMu           *sync.Mutex
}

type ConnectionOptions struct {
	UseCompression bool
	UseSSL         bool
	Proxy          func(*http.Request) (*url.URL, error)
	Subprotocols   []string
}

// todo Yet to be done
type ReconnectionOptions struct {
	Times    int
	Interval time.Duration
}

var reconnectFlag int32 = 0

func New(url string) Socket {
	return Socket{
		Url:           url,
		RequestHeader: http.Header{},
		ConnectionOptions: ConnectionOptions{
			UseCompression: false,
			UseSSL:         true,
		},
		ReconnectionOptions: ReconnectionOptions{Times: 0, Interval: 1 * time.Second},
		WebsocketDialer:     &websocket.Dialer{},
		Timeout:             0,
		sendMu:              &sync.Mutex{},
		receiveMu:           &sync.Mutex{},
	}
}

func (socket *Socket) setConnectionOptions() {
	socket.WebsocketDialer.EnableCompression = socket.ConnectionOptions.UseCompression
	socket.WebsocketDialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: socket.ConnectionOptions.UseSSL}
	socket.WebsocketDialer.Proxy = socket.ConnectionOptions.Proxy
	socket.WebsocketDialer.Subprotocols = socket.ConnectionOptions.Subprotocols
}
func (socket *Socket) DoConnect() (err error) {
	var resp *http.Response
	socket.setConnectionOptions()

	socket.Conn, resp, err = socket.WebsocketDialer.Dial(socket.Url, socket.RequestHeader)

	if err != nil {
		logger.Error.Println("Error while connecting to server ", err)
		if resp != nil {
			logger.Error.Println("HTTP Response %d status: %s", resp.StatusCode, resp.Status)
		}
		socket.IsConnected = false
		if socket.OnConnectError != nil {
			socket.OnConnectError(err, *socket)
		}
		return err
	}

	logger.Info.Println("Connected to server")
	socket.IsConnected = true
	if socket.OnConnected != nil {
		socket.OnConnected(*socket)
	}
	return
}

func (socket *Socket) Reconnect() (err error) {
	if !atomic.CompareAndSwapInt32(&reconnectFlag, 0, 1) {
		return
	}

	if socket.IsConnected {
		return
	}

	reconnectCnt := 0
	for {
		time.Sleep(socket.ReconnectionOptions.Interval)

		reconnectCnt++
		err = socket.DoConnect()

		if socket.ReconnectionOptions.Times > 0 && reconnectCnt >= socket.ReconnectionOptions.Times {
			break
		}

		if err != nil {
			continue
		}

		break
	}

	atomic.CompareAndSwapInt32(&reconnectFlag, 1, 0)

	socket.IsConnected = true
	return
}

func (socket *Socket) Connect() {
	err := socket.DoConnect()

	if err != nil {
		return
	}

	socket.bind()
	go socket.recv()
}

func (socket *Socket) bind() {
	defaultPingHandler := socket.Conn.PingHandler()
	socket.Conn.SetPingHandler(func(appData string) error {
		logger.Trace.Println("Received PING from server")
		if socket.OnPingReceived != nil {
			socket.OnPingReceived(appData, *socket)
		}
		return defaultPingHandler(appData)
	})

	defaultPongHandler := socket.Conn.PongHandler()
	socket.Conn.SetPongHandler(func(appData string) error {
		logger.Trace.Println("Received PONG from server")
		if socket.OnPongReceived != nil {
			socket.OnPongReceived(appData, *socket)
		}
		return defaultPongHandler(appData)
	})

	defaultCloseHandler := socket.Conn.CloseHandler()
	socket.Conn.SetCloseHandler(func(code int, text string) error {
		result := defaultCloseHandler(code, text)
		logger.Warning.Println("Disconnected from server ", result)
		if socket.OnDisconnected != nil {
			socket.IsConnected = false
			socket.OnDisconnected(errors.New(text), *socket)
		}
		return result
	})
}

func (socket *Socket) recv() {
	for {
		socket.receiveMu.Lock()
		if socket.Timeout != 0 {
			socket.Conn.SetReadDeadline(time.Now().Add(socket.Timeout))
		}
		messageType, message, err := socket.Conn.ReadMessage()
		socket.receiveMu.Unlock()
		if err != nil {
			logger.Error.Println("read:", err)
			if socket.OnDisconnected != nil {
				socket.IsConnected = false
				socket.OnDisconnected(err, *socket)
			}
			socket.Reconnect()
			continue
		}
		logger.Info.Println("recv: %s", message)

		switch messageType {
		case websocket.TextMessage:
			if socket.OnTextMessage != nil {
				socket.OnTextMessage(string(message), *socket)
			}
		case websocket.BinaryMessage:
			if socket.OnBinaryMessage != nil {
				socket.OnBinaryMessage(message, *socket)
			}
		}
	}
}

func (socket *Socket) SendText(message string) error {
	err := socket.send(websocket.TextMessage, []byte (message))
	if err != nil {
		logger.Error.Println("write:", err)
	}
	return err
}

func (socket *Socket) SendBinary(data []byte) error {
	err := socket.send(websocket.BinaryMessage, data)
	if err != nil {
		logger.Error.Println("write:", err)
	}
	return err
}

func (socket *Socket) send(messageType int, data []byte) error {
	socket.sendMu.Lock()
	err := socket.Conn.WriteMessage(messageType, data)
	if err != nil {
		logger.Error.Println("send:", err)
		if socket.OnDisconnected != nil {
			socket.IsConnected = false
			socket.OnDisconnected(err, *socket)
		}
		socket.Reconnect()

		if socket.IsConnected {
			socket.Conn.WriteMessage(messageType, data)
		}
	}
	socket.sendMu.Unlock()
	return err
}

func (socket *Socket) close() error {
	err := socket.send(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	if err != nil {
		logger.Error.Println("write close:", err)
	}
	socket.Conn.Close()
	return err
}

func (socket *Socket) Close() {
	err := socket.close()
	if socket.OnDisconnected != nil {
		socket.IsConnected = false
		socket.OnDisconnected(err, *socket)
	}
}
