package app

import (
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

type commandReply struct {
	server    *Server
	client    *transport.ClientConn
	requestID string
	action    string
	paneID    string
	audited   *protocol.Inbound
}

func (s *Server) reply(client *transport.ClientConn, requestID, action, paneID string) commandReply {
	return commandReply{server: s, client: client, requestID: requestID, action: action, paneID: paneID}
}

func (s *Server) auditedReply(client *transport.ClientConn, inbound protocol.Inbound, paneID string) commandReply {
	reply := s.reply(client, inbound.RequestID, inbound.Type, paneID)
	reply.audited = &inbound
	return reply
}

func (r commandReply) fail(message string) {
	r.send(false, "failed", message, nil)
}

func (r commandReply) failWithData(message string, data any) {
	r.send(false, "failed", message, data)
}

func (r commandReply) complete(data any) {
	r.send(true, "completed", "", data)
}

func (r commandReply) schedule(data any) {
	r.send(true, "scheduled", "", data)
}

func (r commandReply) send(ok bool, phase, publicError string, data any) {
	result := &coordinator.CommandResult{
		RequestID: r.requestID,
		Action:    r.action,
		OK:        ok,
		Phase:     phase,
		Error:     publicError,
		PaneID:    r.paneID,
		Data:      data,
	}
	if r.audited != nil {
		r.server.recordWriteAudit(r.client, *r.audited, result)
	}
	r.server.hub.Send(r.client, coordinator.CommandResultMessage(result))
}
