package golf

import (
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Compression type to use for GELF messages that are sent
const (
	COMP_NONE = iota // No compression
	COMP_GZIP        // gzip compression
	COMP_ZLIB        // zlib compression
)

type Client struct {
	hostname string

	conn net.Conn

	chnk *chunker

	queue      []*Message
	queueMutex sync.Mutex

	msgChan  chan *Message
	queueCtl chan int
	sendCtl  chan int

	gz *sync.Pool
	zz *sync.Pool

	config ClientConfig
}

// Configuration used when creating a server instance
type ClientConfig struct {
	ChunkSize   int // The data size for each chunk sent to the server
	Compression int // Compression to use for messagec.
}

/*
Create a new Client instance with the default values for ClientConfig:

 {
	ChunkSize: 1420,
	Compression: COMP_GZIP,
 }
*/
func NewClient() (*Client, error) {
	cc := ClientConfig{
		ChunkSize:   1420,
		Compression: COMP_GZIP,
	}
	return NewClientWithConfig(cc)
}

// Create a new Client instance with the given ClientConfig
func NewClientWithConfig(config ClientConfig) (*Client, error) {
	c := &Client{
		config: config,
		queue:  make([]*Message, 0),

		msgChan:  make(chan *Message, 500),
		queueCtl: make(chan int),
		sendCtl:  make(chan int),
	}

	host, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	c.hostname = host

	return c, nil
}

// Connect to a GELF server at the given URI.
func (c *Client) Dial(uri string) error {
	parsedUri, err := url.Parse(uri)
	if err != nil {
		return err
	}

	if !strings.Contains(parsedUri.Host, ":") {
		parsedUri.Host = parsedUri.Host + ":12201"
	}

	switch parsedUri.Scheme {
	case "udp":
	case "tcp":
	default:
		return errors.New("Unsupported scheme provided")
	}

	switch parsedUri.Query().Get("compress") {
	case "none":
		c.config.Compression = COMP_NONE
	case "zlib":
		c.config.Compression = COMP_ZLIB
	case "gzip":
		c.config.Compression = COMP_GZIP
	}

	conn, err := net.Dial(parsedUri.Scheme, parsedUri.Host)
	if err != nil {
		return err
	}
	c.conn = conn

	c.chnk, err = newChunker(c.conn, c.config.ChunkSize)
	if err != nil {
		return err
	}

	c.gz = &sync.Pool{
		New: func() interface{} {
			gz, _ := gzip.NewWriterLevel(c.chnk, gzip.DefaultCompression)
			return gz
		},
	}

	c.zz = &sync.Pool{
		New: func() interface{} {
			zz, _ := zlib.NewWriterLevel(c.chnk, zlib.DefaultCompression)
			return zz
		},
	}

	go c.queueReceiver()
	go c.msgSender()

	return nil
}

// Close the connection to the server. This call will block until all the
// currently queued messages for the client are sent.
func (c *Client) Close() error {
	if c.conn == nil {
		// Already shut down so it doesn't need to run again
		return nil
	}

	// First quit the queue and wait for it to respond
	// that it's quit
	c.queueCtl <- 1
	for {
		quitVal := <-c.queueCtl
		if quitVal == 2 {
			break
		}
		c.queueCtl <- quitVal
	}

	// Then quit the sender and wait for it to respond
	// that it's quit
	c.sendCtl <- 1
	for {
		quitVal := <-c.sendCtl
		if quitVal == 2 {
			break
		}
		c.sendCtl <- quitVal
	}

	err := c.conn.Close()
	if err != nil {
		return err
	}
	c.conn = nil

	return nil
}

// Queue the given message at the end of the message queue
func (c *Client) QueueMsg(msg *Message) error {
	if msg.Timestamp == nil {
		curTime := time.Now()
		msg.Timestamp = &curTime
	}

	c.msgChan <- msg
	return nil
}

func (c *Client) queueReceiver() {
	for {
		select {
		case msg := <-c.msgChan:
			c.queueMutex.Lock()
			c.queue = append(c.queue, msg)
			c.queueMutex.Unlock()
		case quitVal := <-c.queueCtl:
			if quitVal == 1 {
				// Don't quit if there are still
				// messages in the channel
				if len(c.msgChan) > 0 {
					c.queueCtl <- 1
					continue
				}
				c.queueCtl <- 2
				return
			}
		}
	}
}

func (c *Client) msgSender() {
	var msg *Message
	for {
		c.queueMutex.Lock()
		if len(c.queue) > 0 {
			msg, c.queue = c.queue[0], c.queue[1:]
			c.queueMutex.Unlock()

			data, err := generateMsgJson(msg)
			if err != nil {
				// TODO Not sure what to do at this point? Fail the
				// message silently?
				// Might be able to add an error channel that the
				// user can watch for errors
				continue
			}
			err = c.writeMsg(data, c.conn, c.config.Compression)
			if err != nil {
				// TODO Same as above...
			}
		} else {
			c.queueMutex.Unlock()
			time.Sleep(1 * time.Second)

			select {
			case quitVal := <-c.sendCtl:
				if quitVal == 1 {
					c.queueMutex.Lock()
					if len(c.queue) > 0 {
						c.queueMutex.Unlock()
						c.sendCtl <- 1
						continue
					}
					c.queueMutex.Unlock()
					c.sendCtl <- 2
					return
				}
			default:
			}
		}
	}
}

func (c *Client) writeMsg(data string, w io.Writer, compression int) error {
	defer c.chnk.Flush()

	switch compression {
	case COMP_GZIP:
		gz := c.gz.Get().(*gzip.Writer)
		gz.Write([]byte(data))
		gz.Close()
		gz.Reset(c.chnk)
		c.gz.Put(gz)
	case COMP_ZLIB:
		zz := c.zz.Get().(*zlib.Writer)
		zz.Write([]byte(data))
		zz.Close()
		zz.Reset(c.chnk)
		c.zz.Put(zz)
	default:
		c.chnk.Write([]byte(data))
	}

	return nil
}
