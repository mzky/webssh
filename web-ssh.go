package webssh

import (
	"io"
	"io/ioutil"
	"log"
	"net"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
)

// NewWebSSH 新建对象
func NewWebSSH() *WebSSH {
	return &WebSSH{
		buffSize: 512,
		logger:   log.New(ioutil.Discard, "[webssh] ", log.Ltime|log.Ldate),
	}
}

// WebSSH Websocket和ssh
type WebSSH struct {
	logger   *log.Logger
	store    sync.Map
	buffSize uint32
}
type storeValue struct {
	websocket *websocket.Conn
	conn      net.Conn
}

// SetLogger set logger
func (ws *WebSSH) SetLogger(logger *log.Logger) *WebSSH {
	ws.logger = logger
	return ws
}

// AddWebsocket add websocket connect
func (ws *WebSSH) AddWebsocket(id string, conn *websocket.Conn) {
	ws.logger.Println("add websocket")
	v, loaded := ws.store.LoadOrStore(id, storeValue{websocket: conn})
	if !loaded {
		return
	}
	value := v.(storeValue)
	value.websocket = conn
	ws.logger.Println("ready", value)
	go func() {
		ws.logger.Printf("%s server exit %v", id, ws.server(value))
	}()
}

// AddSSHConn add ssh netword connect
func (ws *WebSSH) AddSSHConn(id string, conn net.Conn) {
	ws.logger.Println("add ssh conn")
	v, loaded := ws.store.LoadOrStore(id, storeValue{conn: conn})
	if !loaded {
		return
	}
	value := v.(storeValue)
	value.conn = conn
	ws.logger.Println("server", value)
	go func() {
		ws.logger.Printf("(%s) server exit %v", id, ws.server(value))
	}()
}

// server 对接ssh和websocket
func (ws *WebSSH) server(value storeValue) error {
	defer value.websocket.Close()
	defer value.conn.Close()

	config := ssh.ClientConfig{
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	var session *ssh.Session
	var stdin io.WriteCloser
	for {
		var msg message
		err := value.websocket.ReadJSON(&msg)
		if err != nil {
			return errors.Wrap(err, "login")
		}
		ws.logger.Println("new message", msg.Type)
		switch msg.Type {
		case messageTypeLogin:
			ws.logger.Printf("login %s", msg.Data)
			config.User = string(msg.Data)
		case messageTypePassword:
			config.Auth = append(config.Auth, ssh.Password(string(msg.Data)))
			session, err = ws.newSSHXtermSession(value.conn, &config)
			if err != nil {
				return errors.Wrap(err, "password")
			}
			defer session.Close()
			stdin, err = session.StdinPipe()
			if err != nil {
				return errors.Wrap(err, "stdin")
			}
			defer stdin.Close()
			err = ws.transferOutput(session, value.websocket)
			if err != nil {
				return errors.Wrap(err, "stdout & stderr")
			}
			err = session.Shell()
			if err != nil {
				return errors.Wrap(err, "shell")
			}
		case messageTypePublickey:
			return errors.New("no support publickey")
		case messageTypeStdin:
			if stdin == nil {
				ws.logger.Println("stdin wait login")
				continue
			}
			_, err = stdin.Write(msg.Data)
			if err != nil {
				return errors.Wrap(err, "write")
			}
		case messageTypeResize:
			if session == nil {
				ws.logger.Println("resize wait session")
				continue
			}
			err = session.WindowChange(msg.Rows, msg.Cols)
			if err != nil {
				return errors.Wrap(err, "resize")
			}
		}
	}
}

// 开始一个ssh xterm回话
func (ws *WebSSH) newSSHXtermSession(conn net.Conn, config *ssh.ClientConfig) (*ssh.Session, error) {
	var err error
	c, chans, reqs, err := ssh.NewClientConn(conn, conn.RemoteAddr().String(), config)
	if err != nil {
		return nil, errors.Wrap(err, "client")
	}
	session, err := ssh.NewClient(c, chans, reqs).NewSession()
	if err != nil {
		return nil, errors.Wrap(err, "session")
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: ws.buffSize, ssh.TTY_OP_OSPEED: ws.buffSize}
	session.RequestPty("xterm", 40, 80, modes)
	return session, nil
}

// 转换标准输出标准错误为消息并发送到websocket
func (ws *WebSSH) transferOutput(session *ssh.Session, conn *websocket.Conn) error {
	ws.logger.Println("transfer")
	stdout, err := session.StdoutPipe()
	if err != nil {
		return errors.Wrap(err, "stdout")
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		errors.Wrap(err, "stderr")
	}
	copyToMessage := func(t messageType, r io.Reader) {
		ws.logger.Println("copy to", t)
		buff := make([]byte, ws.buffSize)
		for {
			n, err := r.Read(buff)
			if err != nil {
				ws.logger.Printf("%s read fail", t)
				return
			}
			err = conn.WriteJSON(&message{Type: t, Data: buff[:n]})
			if err != nil {
				ws.logger.Printf("%s write fail", t)
				return
			}
		}
	}
	go copyToMessage(messageTypeStdout, stdout)
	go copyToMessage(messageTypeStderr, stderr)
	return nil
}