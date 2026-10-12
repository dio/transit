package llmdecider

import (
	"log"

	"github.com/dio/transit/up"
)

// decisionAccessLoggerFactory is the access-logger surface. Envoy calls a logger
// after the stream completes.
//
// Two levels: the config factory parses config once, and the factory it returns
// creates one logger per worker thread.
type decisionAccessLoggerFactory struct{}

func (f *decisionAccessLoggerFactory) Create(
	_ up.AccessLoggerConfigHandle,
	_ []byte,
) (up.AccessLoggerFactory, error) {
	return &decisionLoggerFactory{}, nil
}

type decisionLoggerFactory struct{}

func (f *decisionLoggerFactory) NewLogger() up.AccessLogger { return &decisionAccessLogger{} }
func (f *decisionLoggerFactory) OnDestroy()                 {}

// decisionAccessLogger emits the decision for every completed stream, so a
// routing choice is auditable after the fact rather than only observable as
// which upstream answered.
type decisionAccessLogger struct{ up.EmptyAccessLogger }

func (l *decisionAccessLogger) OnLog(handle up.AccessLoggerHandle, logType up.AccessLogType) {
	if logType != up.AccessLogTypeDownstreamEnd {
		return
	}
	model, ok := handle.GetHeader(up.HttpHeaderTypeRequest, HeaderDecidedModel)
	if !ok && model.ToString() == "" {
		return
	}
	source, _ := handle.GetHeader(up.HttpHeaderTypeRequest, HeaderDecisionSource)
	failure, _ := handle.GetHeader(up.HttpHeaderTypeRequest, HeaderDecisionError)

	log.Printf("llm-decider: decided_model=%q source=%q error=%q status=%d",
		model.ToString(), source.ToString(), failure.ToString(), handle.GetResponseCode())
}
