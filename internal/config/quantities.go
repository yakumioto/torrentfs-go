package config

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"
)

var (
	errByteQuantityRequired = errors.New("must not be empty")
	errByteQuantityFormat   = errors.New("must be an integer followed by a byte unit")
	errByteQuantityUnit     = errors.New("must use a supported byte unit (B, KB, MB, GB, TB, KiB, MiB, GiB, or TiB)")
	errByteQuantityOverflow = errors.New("exceeds the supported byte range")
)

var byteQuantityMultipliers = map[string]int64{
	"B":   1,
	"KB":  1000,
	"MB":  1000 * 1000,
	"GB":  1000 * 1000 * 1000,
	"TB":  1000 * 1000 * 1000 * 1000,
	"KiB": 1 << 10,
	"MiB": 1 << 20,
	"GiB": 1 << 30,
	"TiB": 1 << 40,
}

var byteQuantityUnitsDescending = []struct {
	suffix     string
	multiplier int64
}{
	{suffix: "TiB", multiplier: 1 << 40},
	{suffix: "TB", multiplier: 1000 * 1000 * 1000 * 1000},
	{suffix: "GiB", multiplier: 1 << 30},
	{suffix: "GB", multiplier: 1000 * 1000 * 1000},
	{suffix: "MiB", multiplier: 1 << 20},
	{suffix: "MB", multiplier: 1000 * 1000},
	{suffix: "KiB", multiplier: 1 << 10},
	{suffix: "KB", multiplier: 1000},
}

// ParseByteQuantity parses an integer byte quantity with an explicit SI or IEC
// unit. The returned value is always an exact number of bytes.
func ParseByteQuantity(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, errByteQuantityRequired
	}

	digitEnd := 0
	for digitEnd < len(value) && value[digitEnd] >= '0' && value[digitEnd] <= '9' {
		digitEnd++
	}
	if digitEnd == 0 {
		return 0, errByteQuantityFormat
	}
	if digitEnd == len(value) {
		return 0, errByteQuantityUnit
	}

	suffix := value[digitEnd:]
	if strings.IndexFunc(suffix, unicode.IsSpace) >= 0 || !isASCIILetters(suffix) {
		return 0, errByteQuantityFormat
	}
	multiplier, ok := byteQuantityMultipliers[suffix]
	if !ok {
		return 0, errByteQuantityUnit
	}

	amount, err := strconv.ParseUint(value[:digitEnd], 10, 64)
	if err != nil || amount > uint64(math.MaxInt64/multiplier) {
		return 0, errByteQuantityOverflow
	}
	return int64(amount) * multiplier, nil
}

func isASCIILetters(value string) bool {
	for i := 0; i < len(value); i++ {
		if (value[i] < 'A' || value[i] > 'Z') && (value[i] < 'a' || value[i] > 'z') {
			return false
		}
	}
	return true
}

func formatByteQuantity(value int64) string {
	for _, unit := range byteQuantityUnitsDescending {
		if value >= unit.multiplier && value%unit.multiplier == 0 {
			return strconv.FormatInt(value/unit.multiplier, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(value, 10) + "B"
}

type tomlByteQuantity int64

func (q *tomlByteQuantity) UnmarshalText(text []byte) error {
	value, err := ParseByteQuantity(string(text))
	if err != nil {
		return err
	}
	*q = tomlByteQuantity(value)
	return nil
}
