package rtmp

import (
	"crypto/tls"
	"errors"
	"github.com/elobuff/goamf"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrResponseTimeout = errors.New("rtmp: response timeout")
)

type Client struct {
	url string

	connected bool

	conn net.Conn

	outBytes        uint32
	outMessages     chan *Message
	outWindowSize   uint32
	outChunkSize    uint32
	outChunkStreams map[uint32]*OutboundChunkStream
	inBytes         uint32
	inMessages      chan *Message
	inNotify        chan uint8
	inWindowSize    uint32
	inChunkSize     uint32
	inChunkStreams  map[uint32]*InboundChunkStream

	responses      map[uint32]*Response
	responsesMutex sync.Mutex

	lastTransactionId uint32
	connectionId      string

	amfExternalHandlers map[string]amf.ExternalHandler
}

func NewClient(url string) (c *Client) {
	c = &Client{
		url:                 url,
		amfExternalHandlers: make(map[string]amf.ExternalHandler),
	}

	c.Reset()

	return
}

func (c *Client) IsAlive() bool {
	if c.connected != true {
		return false
	}

	return true
}

func (c *Client) Reset() {
	c.connected = false

	if c.conn != nil {
		c.conn.Close()
	}

	if c.outMessages != nil {
		close(c.outMessages)
	}

	if c.inMessages != nil {
		close(c.inMessages)
	}

	c.outBytes = 0
	c.outMessages = make(chan *Message)
	c.outChunkSize = DEFAULT_CHUNK_SIZE
	c.outWindowSize = DEFAULT_WINDOW_SIZE
	c.outChunkStreams = make(map[uint32]*OutboundChunkStream)
	c.inBytes = 0
	c.inMessages = make(chan *Message)
	c.inChunkSize = DEFAULT_CHUNK_SIZE
	c.inWindowSize = DEFAULT_WINDOW_SIZE
	c.inChunkStreams = make(map[uint32]*InboundChunkStream)
	c.responses = make(map[uint32]*Response)
	c.lastTransactionId = 0
	c.connectionId = ""
}

func (c *Client) RegisterExternalHandler(name string, fn amf.ExternalHandler) {
	c.amfExternalHandlers[name] = fn
}

func (c *Client) Disconnect() {
	c.Reset()
	log.Debug("disconnected from %s", c.url)
}

func (c *Client) Connect() (err error) {
	log.Debug("connecting to %s", c.url)

	url, err := url.Parse(c.url)
	if err != nil {
		return err
	}

	switch url.Scheme {
	case "rtmp":
		c.conn, err = net.DialTimeout("tcp", url.Host, 5*time.Second)
		if err != nil {
			return err
		}
	case "rtmps":
		var nc net.Conn
		nc, err = net.DialTimeout("tcp", url.Host, 5*time.Second)
		if err != nil {
			return err
		}

		config := &tls.Config{InsecureSkipVerify: true}
		tc := tls.Client(nc, config)
		err = tc.Handshake()
		if err != nil {
			return err
		}

		c.conn = tc
	default:
		return Error("Unsupported scheme: %s", url.Scheme)
	}

	log.Debug("handshaking with %s", c.url)

	err = c.handshake()
	if err != nil {
		return Error("client connect: could not complete handshake: %s", err)
	}

	log.Debug("sending connect command to %s", c.url)

	go c.receiveLoop()
	go c.sendLoop()
	go c.routeLoop()

	var id string
	id, err = c.connect()
	if err != nil {
		return Error("client connect: could not complete connect: %s", err)
	}

	c.connected = true
	c.connectionId = id

	log.Debug("connected to %s (%s)", c.url, c.connectionId)

	return
}

func (c *Client) NextTransactionId() uint32 {
	return atomic.AddUint32(&c.lastTransactionId, 1)
}

func (c *Client) GetResponse(tid uint32) (response *Response, ready bool) {
	c.responsesMutex.Lock()
	defer c.responsesMutex.Unlock()
	response = c.responses[tid]
	if response != nil {
		ready = true
		delete(c.responses, tid)
	}
	return
}

func (c *Client) SendMessage(msg *Message) {
	c.outMessages <- msg
}

func (c *Client) Call(msg *Message, t uint32) (response *Response, err error) {
	c.SendMessage(msg)

	tid := msg.TransactionId

	ticker := time.NewTicker(time.Duration(5) * time.Millisecond)
	defer ticker.Stop()

	timeout := time.After(time.Duration(t) * time.Second)

	for {
		select {
		case <-ticker.C:
			res, ready := c.GetResponse(tid)
			if ready {
				return res, nil
			}
		case <-timeout:
			return response, ErrResponseTimeout
		}
	}

	return
}

func (c *Client) Read(p []byte) (n int, err error) {
	n, err = c.conn.Read(p)
	c.inBytes += uint32(n)
	log.Trace("read %d", n)
	return n, err
}

func (c *Client) Write(p []byte) (n int, err error) {
	n, err = c.conn.Write(p)
	c.outBytes += uint32(n)
	log.Trace("write %d", n)
	return n, err
}
