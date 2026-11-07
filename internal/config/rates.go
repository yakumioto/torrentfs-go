package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ByteRate is a transfer rate in bytes per second. It is written with a byte
// unit and an optional "/s" suffix: "2MiB/s" and "2MiB" both mean two
// mebibytes per second. An empty value means the rate is not set.
type ByteRate float64

// UnmarshalText decodes a byte rate from its textual form.
func (r *ByteRate) UnmarshalText(text []byte) error {
	if strings.TrimSpace(string(text)) == "" {
		*r = 0
		return nil
	}
	value, err := ParseByteRate(string(text))
	if err != nil {
		return err
	}
	*r = ByteRate(value)
	return nil
}

// String returns the rate in bytes per second.
func (r ByteRate) String() string { return strconv.FormatFloat(float64(r), 'f', -1, 64) }

// ParseByteRate parses a byte rate. The rate is a byte quantity with an
// optional "/s" suffix.
//
// The function checks the format of the value only: whether the rate is
// positive is a rule of the configuration, not of the format.
func ParseByteRate(raw string) (float64, error) {
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "/s"))
	bytes, err := ParseByteQuantity(value)
	if err != nil {
		return 0, err
	}
	return float64(bytes), nil
}

// Percent is a percentage written as a number, optionally with a "%" suffix:
// "20" and "20%" both mean twenty percent. An empty value means the
// percentage is not set.
type Percent float64

// UnmarshalText decodes a percentage from its textual form.
func (p *Percent) UnmarshalText(text []byte) error {
	if strings.TrimSpace(string(text)) == "" {
		*p = 0
		return nil
	}
	value, err := ParsePercent(string(text))
	if err != nil {
		return err
	}
	*p = Percent(value)
	return nil
}

// String returns the percentage.
func (p Percent) String() string { return strconv.FormatFloat(float64(p), 'f', -1, 64) }

// ParsePercent parses a percentage.
//
// The function checks the format of the value only: whether the percentage
// lies between 0 and 100 is a rule of the configuration, not of the format.
func ParsePercent(raw string) (float64, error) {
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "%"))
	percent, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(percent) || math.IsInf(percent, 0) {
		return 0, fmt.Errorf("must be a number between 0 and 100")
	}
	return percent, nil
}

// Ratio is a multiplicative factor written as a number, optionally with an "x"
// suffix: "2" and "2x" both mean twice. An empty value means the factor is
// not set.
type Ratio float64

// UnmarshalText decodes a ratio from its textual form.
func (r *Ratio) UnmarshalText(text []byte) error {
	if strings.TrimSpace(string(text)) == "" {
		*r = 0
		return nil
	}
	value, err := ParseRatio(string(text))
	if err != nil {
		return err
	}
	*r = Ratio(value)
	return nil
}

// String returns the ratio.
func (r Ratio) String() string { return strconv.FormatFloat(float64(r), 'f', -1, 64) }

// ParseRatio parses a multiplicative factor.
//
// The function checks the format of the value only: whether the factor is
// greater than one is a rule of the configuration, not of the format.
func ParseRatio(raw string) (float64, error) {
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "x"))
	ratio, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return 0, fmt.Errorf("must be a number greater than 1")
	}
	return ratio, nil
}

// OptionalByteQuantity is a byte count whose empty value means "not set".
//
// The quantity types of the configuration reject an empty value, because a
// byte count has no meaningful empty state. The settings of the upload
// statistics experiment do: an unset cap means "uncapped", so the key is
// written with an empty value when it is left out.
type OptionalByteQuantity ByteQuantity

// UnmarshalText decodes a byte quantity from a TOML string.
func (q *OptionalByteQuantity) UnmarshalText(text []byte) error {
	if strings.TrimSpace(string(text)) == "" {
		*q = 0
		return nil
	}
	value, err := ParseByteQuantity(string(text))
	if err != nil {
		return err
	}
	*q = OptionalByteQuantity(value)
	return nil
}

// Int64 returns the quantity as a plain byte count.
func (q OptionalByteQuantity) Int64() int64 { return int64(q) }

// OptionalDuration is a duration whose empty value means "not set".
//
// A duration has no meaningful empty state in the configuration, but the
// settings of the upload statistics experiment do: an unset sampling period
// means the default one.
type OptionalDuration Duration

// UnmarshalText decodes a duration from a TOML string.
func (d *OptionalDuration) UnmarshalText(text []byte) error {
	if strings.TrimSpace(string(text)) == "" {
		*d = 0
		return nil
	}
	value, err := time.ParseDuration(strings.TrimSpace(string(text)))
	if err != nil {
		return fmt.Errorf("invalid duration: %w", err)
	}
	*d = OptionalDuration(value)
	return nil
}

// String returns the duration in Go's standard duration format.
func (d OptionalDuration) String() string { return time.Duration(d).String() }

// formatByteRate renders a byte rate the way a configuration file writes it.
func formatByteRate(value float64) string {
	if value == 0 {
		return "0"
	}
	return formatByteQuantity(int64(value)) + "/s"
}

// formatPercent renders a percentage the way a configuration file writes it.
func formatPercent(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// formatRatio renders a ratio the way a configuration file writes it.
func formatRatio(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// String returns the quantity with a byte unit, the way a configuration file
// writes it.
func (q OptionalByteQuantity) String() string { return formatByteQuantity(int64(q)) }
