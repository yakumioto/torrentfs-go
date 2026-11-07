package config

// Source names the input a configuration value came from.
type Source string

// The sources of a configuration value. They are listed in the order of
// precedence: the later source wins over the earlier one.
const (
	// SourceDefault is the built-in default of a configuration key.
	SourceDefault Source = "default"
	// SourceFile is a configuration file.
	SourceFile Source = "file"
	// SourceEnvironment is an environment variable.
	SourceEnvironment Source = "environment"
)

// Sources records where each configuration key took its effective value.
//
// The loader fills the record while it merges the inputs, so a caller can ask
// where a value came from without repeating the merge. The credential
// warning and the configuration audit both read it.
type Sources map[string]Source

// SourceOf reports the source of the configuration key with the given path.
// It reports SourceDefault for a key the record does not mention, because a
// key the loader never touched kept its built-in default.
func (s Sources) SourceOf(path string) Source {
	if source, ok := s[path]; ok {
		return source
	}
	return SourceDefault
}

// FromFile reports whether the configuration key with the given path took its
// value from a configuration file.
func (s Sources) FromFile(path string) bool {
	return s.SourceOf(path) == SourceFile
}

// FromEnvironment reports whether the configuration key with the given path
// took its value from an environment variable.
func (s Sources) FromEnvironment(path string) bool {
	return s.SourceOf(path) == SourceEnvironment
}
