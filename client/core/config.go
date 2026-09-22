package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"peerly/proto"
)

const defaultCheckpointMinutes = 10

var ErrAlreadyRunning = errors.New("peerly is already running on this PC, maybe as another window or as peerly-browser. Close it first")

type WorldSettings struct {
	SavePath          string `json:"save_path"`
	Launch            string `json:"launch"`
	Process           string `json:"process"`
	Include           string `json:"include"`
	CheckpointMinutes int    `json:"checkpoint_minutes"`
	Confirmed         bool   `json:"confirmed"`
	LastRevisionID    string `json:"last_revision_id"`
	LastManifestHash  string `json:"last_manifest_hash"`
	LastFolder        string `json:"last_folder"`
	LastInclude       string `json:"last_include"`
}

func (w *WorldSettings) RecordSync(revisionID string, manifestHash string) {
	w.LastRevisionID = revisionID
	w.LastManifestHash = manifestHash
	w.LastFolder = ExpandPath(w.SavePath)
	w.LastInclude = w.Include
}

func (w WorldSettings) SyncedBefore() bool {
	return w.LastRevisionID != "" && samePath(w.LastFolder, ExpandPath(w.SavePath)) && w.LastInclude == w.Include
}

func (w WorldSettings) CheckpointInterval() time.Duration {
	switch {
	case w.CheckpointMinutes < 0:
		return 0
	case w.CheckpointMinutes == 0:
		return defaultCheckpointMinutes * time.Minute
	}
	return time.Duration(w.CheckpointMinutes) * time.Minute
}

type Config struct {
	ServerURL string                    `json:"server_url"`
	Token     string                    `json:"token"`
	Member    proto.Member              `json:"member"`
	Group     proto.Group               `json:"group"`
	Worlds    map[string]*WorldSettings `json:"worlds"`
}

type ConfigFile struct {
	dir    string
	mutex  sync.Mutex
	config Config
	Notice string
}

func DefaultConfigDir() (string, error) {
	if override := os.Getenv("PEERLY_CONFIG_DIR"); override != "" {
		return override, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "peerly"), nil
}

func LoadConfig(dir string) (*ConfigFile, error) {
	file := &ConfigFile{dir: dir, config: Config{Worlds: map[string]*WorldSettings{}}}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(file.path())
	if errors.Is(err, os.ErrNotExist) {
		return file, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &file.config); err != nil {
		previous := Config{}
		if rawPrevious, readErr := os.ReadFile(file.path() + ".bak"); readErr == nil && json.Unmarshal(rawPrevious, &previous) == nil && previous.Token != "" {
			os.Rename(file.path(), file.path()+".unreadable-"+time.Now().Format("20060102-150405"))
			file.config = previous
			if file.config.Worlds == nil {
				file.config.Worlds = map[string]*WorldSettings{}
			}
			file.Notice = "The settings file on this PC was damaged, so the previous copy was restored. Check the Settings of your worlds before hosting."
			return file, nil
		}
		quarantined := file.path() + ".unreadable-" + time.Now().Format("20060102-150405")
		if renameErr := os.Rename(file.path(), quarantined); renameErr != nil {
			return nil, fmt.Errorf("settings file is unreadable and could not be set aside: %w", renameErr)
		}
		file.config = Config{Worlds: map[string]*WorldSettings{}}
		file.Notice = "The settings file on this PC was unreadable and was set aside as " + filepath.Base(quarantined) + ". Join your group again with an invite code."
		return file, nil
	}
	if file.config.Worlds == nil {
		file.config.Worlds = map[string]*WorldSettings{}
	}
	return file, nil
}

func (f *ConfigFile) path() string {
	return filepath.Join(f.dir, "config.json")
}

func (f *ConfigFile) Dir() string {
	return f.dir
}

func (f *ConfigFile) BackupDir(worldID string) string {
	return filepath.Join(f.dir, "backups", worldID)
}

func (f *ConfigFile) TempDir() (string, error) {
	dir := filepath.Join(f.dir, "tmp")
	return dir, os.MkdirAll(dir, 0o750)
}

func (f *ConfigFile) CleanTemp() error {
	return os.RemoveAll(filepath.Join(f.dir, "tmp"))
}

func (f *ConfigFile) Snapshot() Config {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	copied := f.config
	copied.Worlds = map[string]*WorldSettings{}
	for worldID, settings := range f.config.Worlds {
		duplicate := *settings
		copied.Worlds[worldID] = &duplicate
	}
	return copied
}

func (f *ConfigFile) World(worldID string) WorldSettings {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if settings, found := f.config.Worlds[worldID]; found {
		return *settings
	}
	return WorldSettings{}
}

func (f *ConfigFile) Update(mutate func(config *Config)) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	mutate(&f.config)
	return f.save()
}

func (f *ConfigFile) UpdateWorld(worldID string, mutate func(settings *WorldSettings)) error {
	return f.Update(func(config *Config) {
		settings, found := config.Worlds[worldID]
		if !found {
			settings = &WorldSettings{}
			config.Worlds[worldID] = settings
		}
		mutate(settings)
	})
}

func (f *ConfigFile) save() error {
	if err := os.MkdirAll(f.dir, 0o750); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(f.config, "", "  ")
	if err != nil {
		return err
	}
	staged, err := os.CreateTemp(f.dir, "config-*.tmp")
	if err != nil {
		return err
	}
	stagedPath := staged.Name()
	_, writeErr := staged.Write(encoded)
	syncErr := staged.Sync()
	closeErr := staged.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		os.Remove(stagedPath)
		return err
	}
	if err := os.Chmod(stagedPath, 0o600); err != nil {
		os.Remove(stagedPath)
		return err
	}
	if current, err := os.ReadFile(f.path()); err == nil && json.Valid(current) {
		os.WriteFile(f.path()+".bak", current, 0o600)
	}
	if err := os.Rename(stagedPath, f.path()); err != nil {
		os.Remove(stagedPath)
		return err
	}
	return nil
}

func NormalizeServerURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("enter the server address your group uses, for example https://saves.example.com")
	}
	if !strings.Contains(trimmed, "://") {
		host, _, _ := strings.Cut(trimmed, "/")
		hostname, _, _ := strings.Cut(host, ":")
		if hostname == "localhost" || hostname == "127.0.0.1" {
			trimmed = "http://" + trimmed
		} else {
			trimmed = "https://" + trimmed
		}
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("%q is not a valid server address, it should look like https://saves.example.com", raw)
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

var percentVariable = regexp.MustCompile(`%([A-Za-z_][A-Za-z0-9_()]*)%`)

func ExpandPath(raw string) string {
	expanded := percentVariable.ReplaceAllStringFunc(strings.TrimSpace(raw), func(match string) string {
		if value, found := os.LookupEnv(strings.Trim(match, "%")); found {
			return value
		}
		return match
	})
	if runtime.GOOS != "windows" {
		expanded = os.ExpandEnv(expanded)
	}
	expanded = strings.Trim(expanded, `"`)
	if expanded == "~" || strings.HasPrefix(expanded, "~/") || strings.HasPrefix(expanded, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = filepath.Join(home, expanded[1:])
		}
	}
	if expanded == "" {
		return ""
	}
	return filepath.Clean(expanded)
}

func samePath(left string, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func isInside(child string, parent string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (!strings.HasPrefix(relative, "..") && !filepath.IsAbs(relative))
}

func CheckSaveFolder(folder string) error {
	if folder == "" {
		return errors.New("no save folder is set for this world on this PC")
	}
	if strings.Contains(folder, "%") {
		return fmt.Errorf("the save folder %s contains a variable this PC does not know", folder)
	}
	if !filepath.IsAbs(folder) {
		return fmt.Errorf("the save folder must be a full path such as C:\\Users\\you\\AppData\\..., got %s", folder)
	}
	cleaned := filepath.Clean(folder)
	tooBroad := fmt.Errorf("%s is not a game save folder, it holds far more than one world. Pick the folder where the game keeps its worlds", cleaned)
	if filepath.Dir(cleaned) == cleaned {
		return tooBroad
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if isInside(home, cleaned) {
			return tooBroad
		}
		for _, broad := range []string{
			"Documents", "Desktop", "Downloads", "Pictures", "Music", "Videos", "Saved Games", "OneDrive", "Dropbox", "Google Drive",
			"AppData", "AppData/Local", "AppData/LocalLow", "AppData/Roaming", "Documents/My Games", "OneDrive/Documents", "OneDrive/Desktop",
			".config", ".local", ".local/share", ".steam", ".var", ".var/app", "Library", "Library/Application Support",
		} {
			if samePath(cleaned, filepath.Join(home, filepath.FromSlash(broad))) {
				return tooBroad
			}
		}
	}
	for _, variable := range []string{"SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramData"} {
		if systemDir := os.Getenv(variable); systemDir != "" {
			if samePath(cleaned, systemDir) || (variable == "SystemRoot" && isInside(cleaned, systemDir)) {
				return tooBroad
			}
		}
	}
	return nil
}

var launcherLink = regexp.MustCompile(`^(steam://(rungameid|run)/[0-9]+|com\.epicgames\.launcher://apps/[A-Za-z0-9%:._-]+\?action=launch(&silent=true)?)$`)

func IsLauncherLink(launch string) bool {
	return launcherLink.MatchString(strings.TrimSpace(launch))
}

var anyLink = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

func IsLink(launch string) bool {
	return anyLink.MatchString(strings.TrimSpace(launch))
}

type Resolved struct {
	WorldSettings
	Folder           string `json:"folder"`
	LaunchSuggestion string `json:"launch_suggestion"`
}

func ResolveSettings(world proto.World, local WorldSettings) Resolved {
	resolved := Resolved{WorldSettings: local}
	if !local.Confirmed {
		if resolved.SavePath == "" {
			resolved.SavePath = world.DefaultSavePath
		}
		if resolved.Process == "" {
			resolved.Process = world.DefaultProcess
		}
		if resolved.Include == "" {
			resolved.Include = world.DefaultInclude
		}
		if resolved.Launch == "" {
			if IsLauncherLink(world.DefaultLaunch) {
				resolved.Launch = strings.TrimSpace(world.DefaultLaunch)
			} else {
				resolved.LaunchSuggestion = world.DefaultLaunch
			}
		}
	}
	resolved.Folder = ExpandPath(resolved.SavePath)
	return resolved
}
