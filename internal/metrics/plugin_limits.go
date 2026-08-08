package metrics

import "unicode/utf8"

const (
	maxPluginCounterNames = 64
	maxPluginMetricNames  = 64
	maxPluginMetricSeries = 64
	maxPluginMetricLabels = 8
	maxPluginLabelValue   = 128

	pluginCounterOverflow = "_rejected_updates"
)

// validPluginTelemetryName implements the public plugin telemetry grammar.
// It is deliberately ASCII-only: these names become JSON keys or OTel
// instrument/attribute names, where visually confusable Unicode is harmful.
func validPluginTelemetryName(name string) bool {
	if len(name) == 0 || len(name) > 64 || !asciiAlphaNumeric(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !asciiAlphaNumeric(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func validPluginLabelValue(value string) bool {
	return len(value) <= maxPluginLabelValue && utf8.ValidString(value)
}
