package player

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

var (
	baseSockArgs     = []string{"--idle", "--terminal=no", "--no-video"}
	ipcArg           = "--input-ipc-server=%s"
	socketTimeout    = time.Second * 2
	socketSleepRetry = time.Millisecond * 10

	ErrCtxCancel         = errors.New("context canceled")
	ErrSocketFileTimeout = errors.New("mpv socket file timeout")
	ErrNoMetadata        = errors.New("no metadata")
)

type ipcCmd uint8

const (
	play ipcCmd = iota
	stop
	pause
	unpause
	volume
	metadata
	mediaTitle
	playbackTime
	quit
)

var ipcCmds = map[ipcCmd]string{
	play:         `["loadfile", "%s","replace"]`,
	stop:         `[ "stop"]`,
	pause:        `["set_property", "pause", true]`,
	unpause:      `["set_property", "pause", false]`,
	volume:       `["set_property", "volume", "%d"]`,
	metadata:     `["get_property_string", "metadata"]`,
	mediaTitle:   `["get_property", "media-title"]`,
	playbackTime: `["get_property", "playback-time"]`,
	quit:         `[ "quit"]`,
}

type MpvSocket struct {
	sockFile string
	conn     net.Conn

	cmd *exec.Cmd
}

func NewMPVSocket(ctx context.Context) (*MpvSocket, error) {
	mpv := &MpvSocket{
		sockFile: fmt.Sprintf(sockFile, os.Getpid()),
	}

	cmd, err := mpvCmd(mpv.sockFile)
	if err != nil {
		return nil, err
	}
	mpv.cmd = cmd

	start := time.Now()
loop:
	for {
		select {
		case <-ctx.Done():
			return nil, ErrCtxCancel
		case <-time.After(socketTimeout):
			return nil, ErrSocketFileTimeout
		default:
			if _, err := os.Stat(mpv.sockFile); os.IsNotExist(err) {
				time.Sleep(socketSleepRetry)
			} else {
				break loop
			}
		}
	}
	slog.Info(fmt.Sprintf("mpv socket file created after %v", time.Since(start)))

	conn, err := getConn(ctx, mpv.sockFile)
	if err != nil {
		return nil, err
	}
	mpv.conn = conn

	return mpv, nil
}

func mpvCmd(sockFile string) (*exec.Cmd, error) {
	log := slog.With("method", "mpvCmd")
	args := slices.Clone(baseSockArgs)
	args = append(args, fmt.Sprintf(ipcArg, sockFile))
	cmd := exec.Command(baseCmd, args...)
	if errors.Is(cmd.Err, exec.ErrDot) {
		cmd.Err = nil
	} else if cmd.Err != nil {
		log.Error("mpv cmd error", "error", cmd.Err.Error())
		return nil, cmd.Err
	}
	err := cmd.Start()
	if err != nil {
		log.Error("mpv cmd start", "error", err)
		return nil, err
	}
	log.Info("mpv cmd started", "pid", cmd.Process.Pid)
	return cmd, nil
}

func (mpv *MpvSocket) Pause(value bool) error {
	log := slog.With("method", "MpvSocket.Pause")
	log.Info("pause", "value", value)
	cmd := ipcCmds[pause]
	if !value {
		cmd = ipcCmds[unpause]
	}
	_, err := mpv.ipcRequest(cmd)
	return err
}

func (mpv *MpvSocket) Play(url string) error {
	log := slog.With("method", "MpvSocket.Play")
	log.Info("playing url=" + url)

	// first unpause, otherwise will start paused
	err := mpv.Pause(false)
	if err != nil {
		return err
	}

	playCmd := fmt.Sprintf(ipcCmds[play], url)
	_, err = mpv.ipcRequest(playCmd)
	return err
}

type icyMetadata struct {
	Notice1     string `json:"icy-notice1"`
	Notice2     string `json:"icy-notice2"`
	Name        string `json:"icy-name"`
	Genre       string `json:"icy-genre"`
	BitRate     string `json:"icy-br"`
	Sr          string `json:"icy-sr"`
	URL         string `json:"icy-url"`
	Pub         string `json:"icy-pub"`
	Description string `json:"icy-description"`
	Title       string `json:"icy-title"`
}

func (mpv *MpvSocket) Metadata() *Metadata {
	m := mpv.getMetadata()
	// TODO? alternate title
	// if m.Err != nil || len(m.Title) == 0 {
	// 	m = mpv.getMediaTitle()
	// }
	cmd := ipcCmds[playbackTime]
	res, _ := mpv.ipcRequest(cmd)
	if res != nil {
		if resF, ok := res.(float64); ok {
			intV := int64(resF)
			if intV < 0 {
				intV = 0
			}
			m.PlaybackTimeSec = &intV
		}
	}
	return &m
}

func (mpv *MpvSocket) getMetadata() Metadata {
	cmd := ipcCmds[metadata]
	res, err := mpv.ipcRequest(cmd)
	if err != nil {
		return Metadata{Err: err}
	}
	resS, ok := res.(string)
	if !ok {
		return Metadata{Err: ErrNoMetadata}
	}
	if len(resS) == 0 {
		return Metadata{Err: ErrNoMetadata}
	}
	var m icyMetadata
	err = json.Unmarshal([]byte(resS), &m)
	if err != nil {
		return Metadata{Err: fmt.Errorf("metadata unmarhsal err: %v", err.Error())}
	}
	return Metadata{Title: strings.TrimSpace(m.Title)}
}

func (mpv *MpvSocket) getMediaTitle() Metadata {
	cmd := ipcCmds[mediaTitle]
	res, err := mpv.ipcRequest(cmd)
	if err != nil {
		return Metadata{Err: err}
	}
	return Metadata{
		Title: strings.TrimSpace(res.(string)),
	}
}

func (mpv *MpvSocket) SetVolume(value int) (int, error) {
	log := slog.With("method", "MpvSocket.SetVolume")
	if value < 0 {
		value = 0
	} else if value > 100 {
		value = 100
	}
	log.Info("volume", "value", value)
	cmd := fmt.Sprintf(ipcCmds[volume], value)
	_, err := mpv.ipcRequest(cmd)
	return value, err
}

func (mpv *MpvSocket) Stop() error {
	log := slog.With("method", "MpvSocket.Stop")
	log.Info("stopping")
	stopCmd := ipcCmds[stop]
	_, err := mpv.ipcRequest(stopCmd)
	return err
}

func (mpv *MpvSocket) Close() (err error) {
	log := slog.With("method", "MpvSocket.Close")
	log.Info("stopping")

	defer func() {
		if mpv.conn == nil {
			return
		}
		closeErr := mpv.conn.Close()
		if closeErr != nil && err == nil {
			log.Error("mpv socket connection close", "err", closeErr)
			err = closeErr
		}
	}()

	quitCmd := ipcCmds[quit]
	_, err = mpv.ipcRequest(quitCmd)
	return err
}

type ipcResp struct {
	Id    int    `json:"request_id"`
	Error string `json:"error"`
	Data  any    `json:"data"`
}

const (
	iprRespSuccess = "success"
)

func (mpv *MpvSocket) ipcRequest(command string) (any, error) {
	log := slog.With("method", "MpvSocket.ipcRequest")
	id := rand.IntN(999) + 1
	cmd := fmt.Sprintf("{ \"command\": %s, \"request_id\": %d }\n", command, id)
	log.Info("ipc", "cmd", cmd)
	_, err := mpv.conn.Write([]byte(cmd))
	if err != nil {
		return nil, fmt.Errorf("ipc write err: %w", err)
	}
	scanner := bufio.NewScanner(mpv.conn)
	for scanner.Scan() {
		l := scanner.Bytes()
		log.Info(fmt.Sprintf("ipc resp=%s", l))
		var res ipcResp
		err := json.Unmarshal(l, &res)
		if err != nil {
			continue
		} else if res.Id != id {
			continue
		} else if res.Error != iprRespSuccess {
			return nil, fmt.Errorf("ipc response error: %s", res.Error)
		}
		return res.Data, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error: %w", err)
	}
	return nil, fmt.Errorf("missing ipc response for command=%q", cmd)
}
