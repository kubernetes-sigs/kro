package view

var _ Viewer = (*HumanView)(nil)
var _ Viewer = (*JSONView)(nil)

type Viewer interface {
	Logger() Logger
}

func NewViewer(vt ViewType, s *Stream, level LogLevel) Viewer {
	// Ensure we never have a nil logger scenario by using appropriate loggers
	switch vt {
	case ViewHuman:
		return NewHumanView(s, level)
	case ViewJSON:
		return NewJSONView(s, level)
	default:
		panic("unknown view type")
	}
}

type HumanView struct {
	*Stream
	logger Logger
}

func NewHumanView(s *Stream, level LogLevel) *HumanView {
	var logger Logger
	if level == LogLevelSilent {
		logger = NewNopLogger()
	} else {
		logger = NewHumanLogger(s.Writer, level)
	}
	return &HumanView{
		Stream: s,
		logger: logger,
	}
}

func (h *HumanView) Logger() Logger {
	return h.logger
}

type JSONView struct {
	*Stream
	logger Logger
}

func NewJSONView(s *Stream, level LogLevel) *JSONView {
	var logger Logger
	if level == LogLevelSilent {
		logger = NewNopLogger()
	} else {
		logger = NewJSONLogger(s.Writer, level)
	}
	return &JSONView{
		Stream: s,
		logger: logger,
	}
}

func (j *JSONView) Logger() Logger {
	return j.logger
}
