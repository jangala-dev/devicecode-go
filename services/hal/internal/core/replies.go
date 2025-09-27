package core

import (
	"devicecode-go/bus"
	"devicecode-go/errcode"
	"devicecode-go/types"
)

func (h *HAL) replyOK(m *bus.Message) {
	if m.CanReply() {
		h.conn.Reply(m, types.OKReply{OK: true}, false)
	}
}

func (h *HAL) replyErr(m *bus.Message, c errcode.Code) {
	if !m.CanReply() {
		return
	}
	if c == "" {
		c = errcode.Error
	}
	h.conn.Reply(m, types.ErrorReply{OK: false, Error: string(c)}, false)
}

func (h *HAL) replyFromError(m *bus.Message, err error) {
	if !m.CanReply() {
		return
	}
	c := errcode.Of(err)
	if c == errcode.OK {
		h.conn.Reply(m, types.OKReply{OK: true}, false)
		return
	}
	h.conn.Reply(m, types.ErrorReply{OK: false, Error: string(c)}, false)
}
