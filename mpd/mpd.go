package mpd

import (
	"bufio"
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Conn represents a connection to the MPD server.
type Conn struct {
	lock    sync.Mutex
	socket  net.Conn
	in      *bufio.Scanner
	out     *bufio.Writer
	version string // protocol version returned by the server
}

type ReplayGainMode int

const (
	ReplayGainOff ReplayGainMode = iota
	ReplayGainTrack
	ReplayGainAlbum
	ReplayGainAuto
)

var ackErrorPattern = regexp.MustCompile(`^ACK \[(\d+)@(\d+)\] \{(.*)\} (.*)$`)
var patternLock sync.Mutex

// Connect() connects to a running MPD instance.
func Connect(addr string) (conn *Conn, err error) {
	conn = new(Conn)
	conn.socket, err = net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	conn.in = bufio.NewScanner(conn.socket)
	conn.in.Split(bufio.ScanLines)
	if ok := conn.in.Scan(); !ok {
		err := conn.in.Err()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	resp := conn.in.Text()
	if !strings.HasPrefix(resp, "OK MPD ") {
		return nil, fmt.Errorf("unexpected MPD response: '%s'", resp)
	}
	conn.version = resp[7:]
	if conn.version == "" {
		return nil, errors.New("MPD reported empty version number")
	}
	conn.out = bufio.NewWriter(conn.socket)
	return conn, nil
}

// Version() returns the version of the protocol that was returned
// when the connection was initially made.
func (conn *Conn) Version() string {
	return conn.version
}

// Send() is a low-level function for sending a raw command to the
// MPD server. It should not end in a newline. This method should only
// be used if none of the other methods will do what you want.
func (conn *Conn) Send(cmd string) (*list.List, error) {
	conn.lock.Lock()
	defer conn.lock.Unlock()

	conn.out.WriteString(cmd + "\n")
	conn.out.Flush()
	resp := list.New()
	for {
		if ok := conn.in.Scan(); !ok {
			err := conn.in.Err()
			if err == nil {
				err = io.EOF
			}
			return nil, err
		}
		line := conn.in.Text()
		if line == "OK" {
			return resp, nil
		} else if strings.HasPrefix(line, "ACK ") {
			return nil, newAckError(line)
		}
	}
}

// SendList() is like Send(), but sends all of the commands at once
// between command_list_begin and command_list_end.
func (conn *Conn) SendList(cmds []string) (*list.List, error) {
	var buffer bytes.Buffer
	buffer.WriteString("command_list_begin\n")
	for _, cmd := range cmds {
		buffer.WriteString(cmd + "\n")
	}
	buffer.WriteString("command_list_end")
	return conn.Send(buffer.String())
}

func (conn *Conn) SetConsume(consume bool) error {
	_, err := conn.Send("consume " + binaryBool(consume))
	return err
}

// TODO: support floats?
func (conn *Conn) SetCrossfade(seconds int64) error {
	_, err := conn.Send("crossfade " + strconv.FormatInt(seconds, 10))
	return err
}

// TODO: support mixramp?

func (conn *Conn) SetRandom(random bool) error {
	_, err := conn.Send("random " + binaryBool(random))
	return err
}

func (conn *Conn) SetRepeat(repeat bool) error {
	_, err := conn.Send("repeat " + binaryBool(repeat))
	return err
}

func (conn *Conn) SetVolume(vol int64) error {
	if vol < 0 || vol > 100 {
		return fmt.Errorf("volume level %d is outside valid range of 0-100", vol)
	}
	_, err := conn.Send("crossfade " + strconv.FormatInt(vol, 10))
	return err
}

func (conn *Conn) SetSingle(single bool) error {
	_, err := conn.Send("single " + binaryBool(single))
	return err
}

func (conn *Conn) SetReplayGainMode(mode ReplayGainMode) error {
	var modeString string
	switch mode {
	case ReplayGainOff:
		modeString = "off"
	case ReplayGainTrack:
		modeString = "track"
	case ReplayGainAlbum:
		modeString = "album"
	case ReplayGainAuto:
		modeString = "auto"
	default:
		return fmt.Errorf("unknown replay gain mode '%d'", mode)
	}
	_, err := conn.Send("replay_gain_mode " + modeString)
	return err
}

func (conn *Conn) Ping() error {
	_, err := conn.Send("ping")
	return err
}

func (conn *Conn) Close() error {
	_, err := conn.Send("close")
	return err
}

type Ack int

const (
	ACK_ERROR_NOT_LIST   Ack = 1
	ACK_ERROR_ARG        Ack = 2
	ACK_ERROR_PASSWORD   Ack = 3
	ACK_ERROR_PERMISSION Ack = 4
	ACK_ERROR_UNKNOWN    Ack = 5

	ACK_ERROR_NO_EXIST       Ack = 50
	ACK_ERROR_PLAYLIST_MAX   Ack = 51
	ACK_ERROR_SYSTEM         Ack = 52
	ACK_ERROR_PLAYLIST_LOAD  Ack = 53
	ACK_ERROR_UPDATE_ALREADY Ack = 54
	ACK_ERROR_PLAYER_SYNC    Ack = 55
	ACK_ERROR_EXIST          Ack = 56
)

// AckError represents an error returned by MPD.
type AckError struct {
	errNum         Ack
	commandNum     int
	currentCommand string
	message        string
}

func (err *AckError) CurrentCommand() string {
	return err.currentCommand
}

func (err *AckError) Error() string {
	return fmt.Sprintf("%d: %s", err.errNum, err.message)
}

// newAckError() parses an ACK error line into an AckError. It panics if
// the line isn't well-formed.
func newAckError(str string) *AckError {
	patternLock.Lock()
	defer patternLock.Unlock()
	matches := ackErrorPattern.FindStringSubmatch(str)
	if matches == nil {
		panic(fmt.Sprintf("couldn't parse ACK error: '%s'", str))
	}
	errNum, _ := strconv.Atoi(matches[1])
	commandNum, _ := strconv.Atoi(matches[2])
	currentCommand := matches[3]
	message := matches[4]
	return &AckError{Ack(errNum), commandNum, currentCommand, message}
}

// binaryBool() converts a boolean value into either "1" or "0".
func binaryBool(b bool) string {
	if b {
		return "1"
	} else {
		return "0"
	}
}
