// Package appcfg resolves configuration and on-disk locations.
package appcfg

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Paths holds every location the program uses.
type Paths struct {
	ConfigDir   string
	DataDir     string
	DownloadDir string
	CacheDir    string
}

// ResolvePaths honors CHUANSHU_HOME as a single-root override (used by
// tests and portable installs); otherwise it follows XDG base dirs.
func ResolvePaths() (Paths, error) {
	if home := os.Getenv("CHUANSHU_HOME"); home != "" {
		p := Paths{
			ConfigDir:   home,
			DataDir:     home,
			DownloadDir: filepath.Join(home, "downloads"),
			CacheDir:    filepath.Join(home, "cache"),
		}
		if err := p.Ensure(); err != nil {
			return Paths{}, err
		}
		return p, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		configDir = filepath.Join(homeDir, ".config")
	}
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		dataDir = filepath.Join(homeDir, ".local", "share")
	}
	cacheDir := os.Getenv("XDG_CACHE_HOME")
	if cacheDir == "" {
		cacheDir = filepath.Join(homeDir, ".cache")
	}
	downloadDir := os.Getenv("XDG_DOWNLOAD_DIR")
	if downloadDir == "" {
		downloadDir = filepath.Join(homeDir, "Downloads")
	}
	p := Paths{
		ConfigDir:   filepath.Join(configDir, "chuanshu"),
		DataDir:     filepath.Join(dataDir, "chuanshu"),
		DownloadDir: filepath.Join(downloadDir, "Chuanshu"),
		CacheDir:    filepath.Join(cacheDir, "chuanshu"),
	}
	if err := p.Ensure(); err != nil {
		return Paths{}, err
	}
	return p, nil
}

// Ensure creates all directories.
func (p Paths) Ensure() error {
	for _, d := range []string{p.ConfigDir, p.DataDir, p.DownloadDir, p.CacheDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// ConfigFile is the config.toml location.
func (p Paths) ConfigFile() string { return filepath.Join(p.ConfigDir, "config.toml") }

// SocketPath is the daemon IPC unix socket.
func (p Paths) SocketPath() string { return filepath.Join(p.DataDir, "chuanshu.sock") }

// Config is the user-editable configuration.
type Config struct {
	Nickname         string `json:"nickname"`
	TCPPort          int    `json:"tcp_port"`
	DiscoveryUDPPort int    `json:"discovery_udp_port"`
	DownloadDir      string `json:"download_dir"`
}

// DefaultConfig derives defaults from the OS user and host.
func DefaultConfig() Config {
	nick := os.Getenv("USER")
	if u, err := os.UserHomeDir(); err == nil && nick == "" {
		nick = filepath.Base(u)
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		if nick == "" {
			nick = host
		}
	}
	if nick == "" {
		nick = "chuanshu"
	}
	return Config{Nickname: nick, TCPPort: 24247, DiscoveryUDPPort: 24246}
}

// Load reads config.toml if it exists. The format is deliberately a
// flat key = value subset of TOML so the file stays hand-editable
// without a parser dependency.
func Load(p Paths) (Config, error) {
	cfg := DefaultConfig()
	raw, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), "\"'")
		switch key {
		case "nickname":
			if val != "" {
				cfg.Nickname = val
			}
		case "tcp_port":
			if n, err := strconv.Atoi(val); err == nil && n > 0 && n < 65536 {
				cfg.TCPPort = n
			}
		case "discovery_udp_port":
			if n, err := strconv.Atoi(val); err == nil && n > 0 && n < 65536 {
				cfg.DiscoveryUDPPort = n
			}
		case "download_dir":
			if val != "" {
				cfg.DownloadDir = val
			}
		}
	}
	if cfg.DownloadDir != "" {
		p2 := p
		_ = p2
	}
	return cfg, nil
}

// Save writes the config back in the same flat format.
func (c Config) Save(p Paths) error {
	body := fmt.Sprintf("# chuanshu config\nnickname = %q\ntcp_port = %d\ndiscovery_udp_port = %d\n", c.Nickname, c.TCPPort, c.DiscoveryUDPPort)
	if c.DownloadDir != "" {
		body += fmt.Sprintf("download_dir = %q\n", c.DownloadDir)
	}
	return os.WriteFile(p.ConfigFile(), []byte(body), 0o644)
}

// EffectiveDownloadDir picks config override over the default.
func (c Config) EffectiveDownloadDir(p Paths) string {
	if c.DownloadDir != "" {
		return c.DownloadDir
	}
	return p.DownloadDir
}
