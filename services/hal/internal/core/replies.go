package core

import (
	"devicecode-go/bus"
	"devicecode-go/errcode"
	"devicecode-go/types"
)

// reply is a unified helper for control replies.
func (h *HAL) reply(m *bus.Message, enqOK bool, code errcode.Code, err error) {
	if !m.CanReply() {
		return
	}
	if err != nil {
		c := errcode.Of(err)
		if c == errcode.OK {
			c = errcode.Error
		}
		h.conn.Reply(m, types.ErrorReply{OK: false, Error: string(c)}, false)
		return
	}
	if enqOK {
		h.conn.Reply(m, types.OKReply{OK: true}, false)
		return
	}
	if code == "" {
		code = errcode.Busy
	}
	h.conn.Reply(m, types.ErrorReply{OK: false, Error: string(code)}, false)
}
