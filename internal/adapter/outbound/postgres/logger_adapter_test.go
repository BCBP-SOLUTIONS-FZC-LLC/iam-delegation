package postgres

import (
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/domain"
	"github.com/stretchr/testify/assert"
)

type fakeMapLogger struct {
	calls []struct {
		level  string
		msg    string
		fields map[string]interface{}
	}
}

func (f *fakeMapLogger) record(level, msg string, fields map[string]interface{}) {
	f.calls = append(f.calls, struct {
		level  string
		msg    string
		fields map[string]interface{}
	}{level, msg, fields})
}

func (f *fakeMapLogger) Debug(msg string, fields map[string]interface{}) {
	f.record("debug", msg, fields)
}
func (f *fakeMapLogger) Info(msg string, fields map[string]interface{}) {
	f.record("info", msg, fields)
}
func (f *fakeMapLogger) Warn(msg string, fields map[string]interface{}) {
	f.record("warn", msg, fields)
}
func (f *fakeMapLogger) Error(msg string, fields map[string]interface{}) {
	f.record("error", msg, fields)
}

// TestLoggerAdapter_ConvertsFieldsToMap verifies LoggerAdapter converts
// pgcommon's domain.Field slice into the map[string]interface{} shape the
// wrapped Logger expects, for every level.
func TestLoggerAdapter_ConvertsFieldsToMap(t *testing.T) {
	fake := &fakeMapLogger{}
	a := NewLoggerAdapter(fake)

	a.Debug("debug msg", domain.Field{Key: "k1", Value: "v1"})
	a.Info("info msg", domain.Field{Key: "k2", Value: 2})
	a.Warn("warn msg", domain.Field{Key: "k3", Value: true})
	a.Error("error msg", domain.Field{Key: "k4", Value: "v4"})

	asrt := assert.New(t)
	asrt.Len(fake.calls, 4)
	asrt.Equal("debug", fake.calls[0].level)
	asrt.Equal("debug msg", fake.calls[0].msg)
	asrt.Equal(map[string]interface{}{"k1": "v1"}, fake.calls[0].fields)
	asrt.Equal(map[string]interface{}{"k2": 2}, fake.calls[1].fields)
	asrt.Equal(map[string]interface{}{"k3": true}, fake.calls[2].fields)
	asrt.Equal(map[string]interface{}{"k4": "v4"}, fake.calls[3].fields)
}

// TestLoggerAdapter_NoFields verifies a call with zero domain.Fields still
// reaches the wrapped Logger with a non-nil, empty map.
func TestLoggerAdapter_NoFields(t *testing.T) {
	fake := &fakeMapLogger{}
	a := NewLoggerAdapter(fake)

	a.Info("no fields")

	assert.Len(t, fake.calls, 1)
	assert.Empty(t, fake.calls[0].fields)
}
