package config

import (
	"errors"
	"testing"
)

func TestParseByteQuantity(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{input: "1B", want: 1},
		{input: "32MB", want: 32_000_000},
		{input: "8GB", want: 8_000_000_000},
		{input: "32MiB", want: 33_554_432},
		{input: " 10MiB ", want: 10 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseByteQuantity(tt.input)
			if err != nil {
				t.Fatalf("ParseByteQuantity(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("ParseByteQuantity(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseByteQuantityRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		input string
		want  error
	}{
		{input: "", want: errByteQuantityRequired},
		{input: "32", want: errByteQuantityUnit},
		{input: "32 MB", want: errByteQuantityFormat},
		{input: "1.5MB", want: errByteQuantityFormat},
		{input: "1e3MB", want: errByteQuantityFormat},
		{input: "+1MB", want: errByteQuantityFormat},
		{input: "-1MB", want: errByteQuantityFormat},
		{input: "32Mb", want: errByteQuantityUnit},
		{input: "32PB", want: errByteQuantityUnit},
		{input: "9223372036854775808B", want: errByteQuantityOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			_, err := ParseByteQuantity(tt.input)
			if !errors.Is(err, tt.want) {
				t.Fatalf("ParseByteQuantity(%q) error = %v, want %v", tt.input, err, tt.want)
			}
		})
	}
}

func TestFormatByteQuantityRoundTrips(t *testing.T) {
	for _, want := range []int64{1, 1234567, 32_000_000, 33_554_432, 8_000_000_000, 10 << 20} {
		formatted := formatByteQuantity(want)
		got, err := ParseByteQuantity(formatted)
		if err != nil {
			t.Fatalf("ParseByteQuantity(formatByteQuantity(%d) = %q): %v", want, formatted, err)
		}
		if got != want {
			t.Fatalf("ParseByteQuantity(formatByteQuantity(%d) = %q) = %d", want, formatted, got)
		}
	}
}
