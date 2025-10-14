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

func (h *HAL) replyErr(m *bus.Message, code errcode.Code) {
	if !m.CanReply() {
		return
	}
	if code == "" {
		code = errcode.Error
	}
	h.conn.Reply(m, types.ErrorReply{OK: false, Error: string(code)}, false)
}
