package cli

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/katbyte/go-kt/clog"
	"github.com/katbyte/go-kt/mcp/server"
	"github.com/katbyte/go-kt/version"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/katbyte/truenas-mcp/tools"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type FlagData struct {
	Server               string `mapstructure:"server"`
	Username             string `mapstructure:"username"`
	Token                string `mapstructure:"token"`
	CertificateFile      string `mapstructure:"certificate-file"`
	CAFile               string `mapstructure:"ca-file"`
	SkipCertificateCheck bool   `mapstructure:"skip-certificate-check"`
	TokenFile            string `mapstructure:"token-file"`
	SendKeyUnchecked     bool   `mapstructure:"send-key-unchecked"`
	NoChannelBinding     bool   `mapstructure:"no-channel-binding"`

	ReadOnly     bool     `mapstructure:"read-only"`
	EnableWrite  bool     `mapstructure:"enable-write"`
	EnableDelete bool     `mapstructure:"enable-delete"`
	Toolsets     []string `mapstructure:"toolsets"`
	AllowTools   []string `mapstructure:"allow-tools"`
	DenyTools    []string `mapstructure:"deny-tools"`
	Listen       string   `mapstructure:"listen"`
	AuthToken    string   `mapstructure:"auth-token"`
	AllowNoAuth  bool     `mapstructure:"allow-no-auth"`
}

// configName is the settings file read from $HOME and from the working directory.
const configName = ".truenas-mcp"

// envNames binds each flag to its environment variable.
var envNames = map[string]string{ //nolint:gosec // G101: these are the names of settings, not what they are set to
	"server":                 "TRUENAS_SERVER",
	"username":               "TRUENAS_USERNAME",
	"token":                  "TRUENAS_TOKEN",
	"certificate-file":       "TRUENAS_CERTIFICATE_FILE",
	"ca-file":                "TRUENAS_CA_FILE",
	"skip-certificate-check": "TRUENAS_SKIP_CERTIFICATE_CHECK",
	"token-file":             "TRUENAS_TOKEN_FILE",
	"send-key-unchecked":     "TRUENAS_SEND_KEY_UNCHECKED",
	"no-channel-binding":     "TRUENAS_NO_CHANNEL_BINDING",
	"read-only":              "TRUENAS_READ_ONLY",
	"enable-write":           "TRUENAS_ENABLE_WRITE",
	"enable-delete":          "TRUENAS_ENABLE_DELETE",
	"toolsets":               "TRUENAS_TOOLSETS",
	"allow-tools":            "TRUENAS_ALLOW_TOOLS",
	"deny-tools":             "TRUENAS_DENY_TOOLS",
	"listen":                 "TRUENAS_LISTEN",
	"auth-token":             "TRUENAS_AUTH_TOKEN",
	"allow-no-auth":          "TRUENAS_ALLOW_NO_AUTH",
}

func configureFlags(root *cobra.Command) error {
	pflags := root.PersistentFlags()

	pflags.StringP("server", "s", "", "the TrueNAS's address, e.g. nas.example.net or https://nas.example.net:8443, or unix:// when run on the NAS itself, which needs no key")
	pflags.StringP("username", "u", "", "the user the API key was made for (without it the key is logged in the older way, which TrueNAS 27 takes away)")
	pflags.StringP("token", "t", "", "the TrueNAS API key (consider exporting to TRUENAS_TOKEN instead)")
	pflags.String("certificate-file", "", "the TrueNAS's own certificate as a PEM file, accepted whatever name is on it: for a NAS still on the certificate it came with")
	pflags.String("ca-file", "", "a PEM file of the certificate authority that signed the TrueNAS's certificate, for a NAS given one made out to its name")
	pflags.Bool("skip-certificate-check", false, "take whatever certificate the TrueNAS shows: nothing then says it is the NAS that answers, and a TrueNAS before version 26 is not sent the key unless --send-key-unchecked says so")
	pflags.String("token-file", "", "a file holding the TrueNAS API key, or a key file as TrueNAS hands one out, in place of --token")
	pflags.Bool("send-key-unchecked", false, "with --skip-certificate-check, send the key to a TrueNAS before version 26 all the same: it takes a key only as it is, so whatever answers at the address is handed it")
	pflags.Bool("no-channel-binding", false, "do not tie the login to the certificate the TrueNAS shows: for a TrueNAS 26 or later reached through a reverse proxy, which shows a certificate of its own")

	pflags.Bool("enable-write", false, "register the tools that change something; without it every tool only reads")
	pflags.Bool("enable-delete", false, "register the tools that delete as well; needs --enable-write")
	pflags.Bool("read-only", false, "register only tools that never change anything: the default, said out loud, and refused beside --enable-write")
	pflags.StringSlice("toolsets", nil, "groups of tools to register: all, "+strings.Join(tools.ToolsetOrder, ", ")+" (core is the default and always included), or a resource family like pool")
	pflags.StringSlice("allow-tools", nil, "tools to register by name, prefix globs like pool_*, or the essential preset: added to the --toolsets given, or with no --toolsets the only tools registered")
	pflags.StringSlice("deny-tools", nil, "never register these tools: names or prefix globs like *_delete; it is how a toolset is narrowed")
	pflags.String("listen", "", "serve MCP over HTTP on this address (e.g. :8080) instead of stdio")
	pflags.String("auth-token", "", "bearer token required on the HTTP endpoint (consider exporting to TRUENAS_AUTH_TOKEN instead)")
	pflags.Bool("allow-no-auth", false, "serve HTTP with no bearer token: anyone who can reach the port can use every tool")

	for name, env := range envNames {
		if err := viper.BindPFlag(name, pflags.Lookup(name)); err != nil {
			return fmt.Errorf("error binding '%s' flag: %w", name, err)
		}

		if err := viper.BindEnv(name, env); err != nil {
			return fmt.Errorf("error binding '%s' to env '%s' : %w", name, env, err)
		}
	}

	readConfigFiles()

	// a config file spells a setting the way the environment does, with or without the prefix - CERTIFICATE_FILE or TRUENAS_CERTIFICATE_FILE for
	// --certificate-file - and viper only matches a key spelled as the flag is, so every two-word setting would be ignored. Carried across as
	// defaults, they still lose to a flag or the environment.
	for name, env := range envNames {
		for _, alt := range []string{strings.ReplaceAll(name, "-", "_"), strings.ToLower(env)} {
			if alt != name && viper.InConfig(alt) {
				viper.SetDefault(name, viper.Get(alt))

				break
			}
		}
	}

	return nil
}

// readConfigFiles reads ~/.truenas-mcp and then ./.truenas-mcp over it, setting by setting: the home file holds the usual settings and a project's file
// changes the ones it names. viper on its own reads only the first file it finds, so a project file that set one thing would lose the address and key
// from home.
func readConfigFiles() {
	viper.SetConfigType("env")

	var files []string
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, configName))
	}
	if wd, err := os.Getwd(); err == nil && (len(files) == 0 || filepath.Join(wd, configName) != files[0]) {
		files = append(files, filepath.Join(wd, configName))
	}

	read := false
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		viper.SetConfigFile(f)
		merge := viper.ReadInConfig
		if read {
			merge = viper.MergeInConfig
		}
		if err := merge(); err != nil {
			clog.Log.Errorf("Error reading config file %s: %v", f, err)

			continue
		}
		read = true
	}
}

// GetFlags returns the fully populated FlagData. It is read from viper, not from the flags themselves, because the flags hold only what was typed on
// the command line and viper lays the environment and the config file over that.
func GetFlags() *FlagData {
	var f FlagData
	if err := viper.Unmarshal(&f); err != nil {
		clog.Log.Fatalf("failed to unmarshal configuration: %v", err)
	}

	return &f
}

// NewConn is a connection to the configured TrueNAS, which says what it does in the process's log at the level TRUENAS_LOG sets, with the key hidden.
// Nothing is dialled until it is first called on.
func (f *FlagData) NewConn() (*truenasrpc.Conn, error) {
	key := f.Token
	if f.TokenFile != "" {
		if key != "" {
			return nil, errors.New("the TrueNAS API key is given twice, as token and as token-file: give one")
		}

		text, err := os.ReadFile(f.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading the TrueNAS API key file: %w", err)
		}
		key = string(text)
	}

	var how []truenasrpc.LoginOption
	if f.SendKeyUnchecked {
		how = append(how, truenasrpc.EvenUnchecked())
	}
	if f.NoChannelBinding {
		how = append(how, truenasrpc.WithoutChannelBinding())
	}

	cred := truenasrpc.APIKey(f.Username, key, how...)
	switch {
	case key == "" && truenasrpc.OverSocket(f.Server):
		// on the NAS itself, over its own socket, the NAS can see who is asking
		cred = truenasrpc.LocalUser()
	case f.Username == "":
		cred = truenasrpc.APIKeyOnly(key, how...)
	}

	opts := []truenasrpc.Option{truenasrpc.WithLog(clog.Log), truenasrpc.WithUserAgent("truenas-mcp/" + version.Version)}
	if f.CertificateFile != "" {
		opts = append(opts, truenasrpc.WithCertificateFile(f.CertificateFile))
	}
	if f.CAFile != "" {
		opts = append(opts, truenasrpc.WithCAFile(f.CAFile))
	}
	if f.SkipCertificateCheck {
		opts = append(opts, truenasrpc.WithoutCertificateCheck())
	}

	return truenasrpc.New(f.Server, cred, opts...)
}

// hinted is a connection's error with the flags that answer it added, where there are any: the connection says what to do in its own words, and a
// person at a command line needs the names of the flags.
func hinted(err error) error {
	login, refused := errors.AsType[*truenasrpc.LoginError](err)
	_, otherName := errors.AsType[x509.HostnameError](err)
	_, untrusted := errors.AsType[*tls.CertificateVerificationError](err)

	switch {
	case errors.Is(err, truenasrpc.ErrNothingVouches):
		return fmt.Errorf("%w (--certificate-file hands over the certificate, --send-key-unchecked sends the key anyway)", err)
	case refused && login.Tied:
		return fmt.Errorf("%w (--no-channel-binding leaves the login untied)", err)
	case otherName:
		return fmt.Errorf("%w (--certificate-file)", err)
	case untrusted:
		return fmt.Errorf("%w (--certificate-file, --ca-file or --skip-certificate-check)", err)
	default:
		return err
	}
}

// DefaultToolsets is what the binary registers when neither --toolsets nor --allow-tools is given: enough to know which NAS this is and nothing that
// writes. Every tool's definition is spent from a client's context before a question is asked, so the rest is asked for with --toolsets, or
// --toolsets all for everything.
var DefaultToolsets = []string{"core"}

// ToolOptions maps the flags onto the tool registration options. An allow list with no --toolsets is the whole choice: it already says which tools to
// load, so the default set is not added to it. Beside --toolsets it adds its tools to the sets.
//
// A session only reads unless --enable-write says otherwise. --read-only is taken because the other katbyte MCP servers are started with it, and here
// says what is already so: beside --enable-write it is a contradiction, and refused.
func (f *FlagData) ToolOptions() (tools.Options, error) {
	if f.ReadOnly && f.EnableWrite {
		return tools.Options{}, errors.New("--read-only and --enable-write contradict each other: give one")
	}
	sets := f.Toolsets
	if len(sets) == 0 && len(f.AllowTools) == 0 {
		sets = DefaultToolsets
	}

	return tools.Options{
		EnableWrite:  f.EnableWrite,
		EnableDelete: f.EnableDelete,
		Toolsets:     sets,
		Allow:        f.AllowTools,
		Deny:         f.DenyTools,
	}, nil
}

// serveOptions is how this tool serves: stdio, or HTTP at --listen behind --auth-token, with the health probe a container asks outside the check.
func (f *FlagData) serveOptions() server.Options {
	return server.Options{
		Listen:      f.Listen,
		AuthToken:   f.AuthToken,
		AllowNoAuth: f.AllowNoAuth,
		Name:        "truenas-mcp",
		EnvPrefix:   "TRUENAS",
	}
}
