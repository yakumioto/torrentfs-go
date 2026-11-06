package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// envLookup reads configuration values from the process environment. It also
// enumerates the variables that are set, so a later stage can reject unknown
// configuration variables without a second environment source.
type envLookup interface {
	// Lookup returns the value of one variable and reports whether it is set.
	Lookup(name string) (string, bool)
	// Names returns the names of every variable that is set.
	Names() []string
}

// osEnvironment reads the process environment.
type osEnvironment struct{}

// Lookup returns the value of one environment variable.
func (osEnvironment) Lookup(name string) (string, bool) { return os.LookupEnv(name) }

// Names returns the names of every environment variable that is set.
func (osEnvironment) Names() []string {
	names := os.Environ()
	result := make([]string, 0, len(names))
	for _, entry := range names {
		if index := strings.IndexByte(entry, '='); index > 0 {
			result = append(result, entry[:index])
		}
	}
	return result
}

// The names of the shared credential pair. The pair has no configuration key
// of its own: it configures the credentials of the HTTP and the SMB service.
const (
	environmentUsername = "TORRENTFS_USERNAME"
	environmentPassword = "TORRENTFS_PASSWORD"
)

// The kinds of exception to the environment name derivation rule.
const (
	// exceptionCredential names a variable that configures no configuration
	// key of its own.
	exceptionCredential = "credential"
	// exceptionExplicitSelection names a derived variable whose value also
	// selects the output backend explicitly.
	exceptionExplicitSelection = "explicit selection"
	// exceptionExcludedKey names a configuration key that the environment
	// does not override.
	exceptionExcludedKey = "excluded key"
)

// EnvironmentException describes one environment variable that does not follow
// the plain derivation rule, and why.
type EnvironmentException struct {
	// Name is the environment variable.
	Name string
	// Kind names the kind of the exception.
	Kind string
	// Path is the configuration key the variable relates to. It is empty for
	// a credential variable, which configures a group of configuration keys.
	Path string
	// Reason explains why the variable is an exception.
	Reason string
	// SelectsOutputBackend reports whether setting the variable selects the
	// output backend explicitly. Such a variable is the derived name of its
	// configuration key: the exception is its effect, not its name.
	SelectsOutputBackend bool
}

// environmentExceptions lists every environment variable that the plain
// derivation rule does not cover.
//
// The table keeps the exceptions enumerable: a reader sees every variable the
// rule does not explain, and the loader applies the same table instead of
// scattering special cases over the code.
var environmentExceptions = []EnvironmentException{
	{
		Kind: exceptionCredential,
		Name: environmentUsername,
		Reason: "the shared credential pair has no configuration key of its own: it " +
			"configures the login name of the HTTP and the SMB service, and the " +
			"unified credentials replace it",
	},
	{
		Kind: exceptionCredential,
		Name: environmentPassword,
		Reason: "the shared credential pair has no configuration key of its own: it " +
			"configures the password of the HTTP and the SMB service, and the " +
			"unified credentials replace it",
	},
	{
		Kind: exceptionExplicitSelection,
		Name: deriveEnvironmentName(outputBackendPath),
		Path: outputBackendPath,
		Reason: "setting it selects the output backend explicitly, so it wins over " +
			"the mountpoint heuristic that would otherwise choose the FUSE output",
		SelectsOutputBackend: true,
	},
	{
		Kind: exceptionExcludedKey,
		Name: deriveEnvironmentName("http.auth.username"),
		Path: "http.auth.username",
		Reason: "the shared credential pair configures the HTTP login name; a second " +
			"source would let the two disagree",
	},
	{
		Kind: exceptionExcludedKey,
		Name: deriveEnvironmentName("http.auth.password_hash"),
		Path: "http.auth.password_hash",
		Reason: "the shared credential pair generates the password hash; a second " +
			"source would let the two disagree",
	},
	{
		Kind: exceptionExcludedKey,
		Name: deriveEnvironmentName("http.auth.password_hash_file"),
		Path: "http.auth.password_hash_file",
		Reason: "the hash file is a configuration file setting only; the unified " +
			"credentials remove it",
	},
}

// EnvironmentExceptions returns every exception to the environment name
// derivation rule. The result is a copy: callers cannot modify the table.
func EnvironmentExceptions() []EnvironmentException {
	exceptions := make([]EnvironmentException, len(environmentExceptions))
	copy(exceptions, environmentExceptions)
	return exceptions
}

// environmentOwns reports whether an exception keeps the environment from
// overriding the configuration key with the given path.
func environmentOwns(path string) bool {
	for _, exception := range environmentExceptions {
		if exception.Kind == exceptionExcludedKey && exception.Path == path {
			return true
		}
	}
	return false
}

// applyEnvironment overlays the environment onto the configuration.
//
// The environment variable of a configuration key is derived from its path, so
// a configuration key needs no registration to become overridable. The
// exceptions to that rule are listed in environmentExceptions.
func applyEnvironment(cfg *Config, lookup envLookup) error {
	if removed := removedEnvironmentNames(lookup); len(removed) > 0 {
		return &RemovedError{Names: removed}
	}
	values, err := applySchema(configSchema, reflect.ValueOf(cfg).Elem(), lookup)
	if err != nil {
		return err
	}
	if selectsOutputBackend(values) {
		// An explicit selection of the output backend wins over the
		// mountpoint heuristic that would otherwise choose the FUSE output.
		cfg.backendExplicit = true
	}
	if !cfg.backendExplicit && (cfg.FUSE.Mountpoint != "" || cfg.FUSE.STRMMountpoint != "") {
		cfg.Filesystem.Backend = BackendFUSE
	}
	if err := applySMBSTRMEnvironment(cfg, values); err != nil {
		return err
	}
	if err := applySMBEnvironmentCredentials(cfg, lookup); err != nil {
		return err
	}
	return applyHTTPAuthenticationEnvironment(cfg, lookup)
}

// selectsOutputBackend reports whether the environment selected the output
// backend explicitly.
func selectsOutputBackend(values map[string]any) bool {
	for _, exception := range environmentExceptions {
		if !exception.SelectsOutputBackend {
			continue
		}
		if _, selected := values[exception.Path]; selected {
			return true
		}
	}
	return false
}

// applySMBEnvironmentCredentials applies the shared credential pair to the SMB
// service. Without the pair, the password comes from the configured password
// file.
func applySMBEnvironmentCredentials(cfg *Config, lookup envLookup) error {
	if cfg.Filesystem.Backend != BackendSMB {
		return nil
	}
	username, userSet := lookup.Lookup(environmentUsername)
	password, passwordSet := lookup.Lookup(environmentPassword)
	if userSet != passwordSet {
		return invalid("smb", fmt.Errorf("%s and %s must be set together",
			environmentUsername, environmentPassword))
	}
	if userSet {
		cfg.SMB.Username, cfg.SMB.Password = username, password
		return nil
	}
	return readSMBPasswordFile(cfg)
}

// applyHTTPAuthenticationEnvironment applies the shared credential pair to the
// HTTP authentication service: the plaintext password becomes an in-memory
// hash, so no password file is read.
func applyHTTPAuthenticationEnvironment(cfg *Config, lookup envLookup) error {
	if !cfg.HTTP.Auth.Enabled || cfg.HTTP.ListenAddr == "" {
		return nil
	}

	username, usernameSet := lookup.Lookup(environmentUsername)
	password, passwordSet := lookup.Lookup(environmentPassword)
	if !usernameSet && !passwordSet {
		return nil
	}
	if !usernameSet || !passwordSet {
		return invalid("http.auth", fmt.Errorf("environment variables %s and %s must be set together",
			environmentUsername, environmentPassword))
	}
	if username == "" {
		return invalid("http.auth.username", fmt.Errorf("environment variable %s must not be empty", environmentUsername))
	}
	if password == "" {
		return invalid("http.auth.password", fmt.Errorf("environment variable %s must not be empty", environmentPassword))
	}
	if len([]byte(password)) > 72 {
		return invalid("http.auth.password", fmt.Errorf("environment variable %s must be at most 72 bytes", environmentPassword))
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return invalid("http.auth.password", fmt.Errorf("environment variable %s could not be hashed", environmentPassword))
	}
	auth := cfg.HTTP.Auth
	auth.Username = username
	auth.PasswordHash = string(hash)
	auth.PasswordHashFile = ""
	cfg.HTTP.Auth = auth
	return nil
}

// parseEnvList splits a comma-separated environment value. Blank entries are
// dropped so a trailing comma is not a configuration error.
func parseEnvList(raw string) []string {
	var values []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			values = append(values, item)
		}
	}
	return values
}

func parseEnvInt(raw string) (int, error) {
	value, err := strconv.ParseInt(raw, 10, 0)
	if err != nil {
		return 0, errors.New("must be a base-10 integer")
	}
	return int(value), nil
}

func parseEnvBool(raw string) (bool, error) {
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New("must be a boolean")
	}
	return value, nil
}

func parseEnvDuration(raw string) (Duration, error) {
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errors.New("must be a Go duration")
	}
	return Duration(value), nil
}
