package api

import "time"

// LogRecord is one of an agent's log records, which it sends with its report: info and
// above, as its own log has them.
type LogRecord struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
	Attrs []LogAttr `json:"attrs,omitempty"`
}

// LogAttr is one of a record's attributes, its value as text. A group's attributes are
// flattened to "group.key".
type LogAttr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}
