package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"peerly/client/core"
	"peerly/proto"
)

const usage = `peerly-cli <command> [flags]

  create-group   -server URL -name GROUP -as YOUR_NAME [-admin-key KEY]
  join           -server URL -code INVITE -as YOUR_NAME
  status
  create-world   -name NAME -game GAME [-save-path P] [-launch CMD] [-process EXE] [-include GLOBS]
  configure      -world NAME [-save-path P] [-launch CMD] [-process EXE] [-include GLOBS] [-checkpoint-minutes N]
  host           -world NAME [-join-info TEXT]
  sync           -world NAME
  history        -world NAME
  pull           -world NAME [-revision ID]
  promote        -world NAME -revision ID
  discard        -revision ID
  keep           -revision ID     never remove this save automatically
  unkeep         -revision ID
  delete-world   -world NAME
  invite                          (owner) print a one-use invite code for one friend
  approve        -member ID       (owner) approve a PC that joined with an invite
  make-owner     -member ID       (owner) hand the group to another member
  remove-member  -member ID
  set-server     -server URL     point this PC at the group's server under a new address
  admin-groups   -server URL -admin-key KEY               list groups on the server
  admin-recover  -server URL -admin-key KEY -group ID -as NAME   become the owner of a group whose owner is gone
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func printSurvey(survey core.Survey) {
	fmt.Printf("folder: %s\n", survey.Folder)
	if survey.Problem != "" {
		fmt.Printf("problem: %s\n", survey.Problem)
		return
	}
	if !survey.FolderExists {
		fmt.Println("the folder does not exist yet on this PC")
		return
	}
	fmt.Printf("%d files (%d KB) belong to this world, %d other files are left alone\n", survey.MatchedCount, survey.MatchedBytes/1024, survey.OtherCount)
	for _, name := range survey.Matched {
		fmt.Println("  +", name)
	}
	if survey.MatchedCount == 0 {
		for _, name := range survey.Others {
			fmt.Println("  -", name)
		}
	}
}

func run(command string, args []string) error {
	configDir, err := core.DefaultConfigDir()
	if err != nil {
		return err
	}
	config, err := core.LoadConfig(configDir)
	if err != nil {
		return err
	}
	if config.Notice != "" {
		fmt.Fprintln(os.Stderr, config.Notice)
	}
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	serverURL := flags.String("server", "", "server url")
	name := flags.String("name", "", "group or world name")
	flags.StringVar(name, "group", "", "group id for admin-recover")
	displayName := flags.String("as", "", "your display name")
	adminKey := flags.String("admin-key", "", "server admin key")
	inviteCode := flags.String("code", "", "invite code")
	gameName := flags.String("game", "", "game name")
	savePath := flags.String("save-path", "", "save folder")
	launch := flags.String("launch", "", "launch command")
	process := flags.String("process", "", "game process name")
	include := flags.String("include", "", "comma separated globs, prefix with ! to exclude")
	checkpointMinutes := flags.Int("checkpoint-minutes", 0, "mid-session backup interval, -1 turns it off")
	worldName := flags.String("world", "", "world name or id")
	revisionID := flags.String("revision", "", "revision id")
	memberID := flags.String("member", "", "member id")
	joinInfo := flags.String("join-info", "", "join code or address shown to friends while you host")
	localChanges := flags.String("local-changes", "branch", "what to do with progress made on this PC since the last sync: branch or latest")
	flags.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	saved := config.Snapshot()
	client := core.NewAPIClient(saved.ServerURL, saved.Token)

	saveSession := func(normalizedURL string, session proto.SessionResponse) error {
		if session.Member.Status == proto.MemberPending {
			fmt.Printf("asked to join %s as %s, waiting for the group owner to approve this PC\n", session.Group.Name, session.Member.DisplayName)
		} else {
			fmt.Printf("created %s as %s, run 'peerly-cli invite' to invite a friend\n", session.Group.Name, session.Member.DisplayName)
		}
		return config.Update(func(stored *core.Config) {
			stored.ServerURL = normalizedURL
			stored.Token = session.Token
			stored.Member = session.Member
			stored.Group = session.Group
		})
	}

	switch command {
	case "admin-groups", "admin-recover":
		normalizedURL, err := core.NormalizeServerURL(*serverURL)
		if err != nil {
			return err
		}
		admin := core.NewAPIClient(normalizedURL, "")
		admin.AdminKey = *adminKey
		if command == "admin-groups" {
			groups, err := admin.AdminGroups(ctx)
			if err != nil {
				return err
			}
			for _, group := range groups {
				fmt.Printf("  %s  %-24s owner %-16s %d members, %d worlds\n", group.ID, group.Name, group.OwnerName, group.MemberCount, group.WorldCount)
			}
			return nil
		}
		session, err := admin.RecoverGroup(ctx, *name, *displayName)
		if err != nil {
			return err
		}
		fmt.Printf("this PC is now the owner of %s as %s\n", session.Group.Name, session.Member.DisplayName)
		return saveSession(normalizedURL, session)
	case "create-group", "join":
		normalizedURL, err := core.NormalizeServerURL(*serverURL)
		if err != nil {
			return err
		}
		anonymous := core.NewAPIClient(normalizedURL, "")
		anonymous.AdminKey = *adminKey
		normalizedURL, err = anonymous.ResolveBaseURL(ctx)
		if err != nil {
			return err
		}
		anonymous.BaseURL = normalizedURL
		var session proto.SessionResponse
		if command == "create-group" {
			session, err = anonymous.CreateGroup(ctx, *name, *displayName)
		} else {
			deviceName, _ := os.Hostname()
			session, err = anonymous.JoinGroup(ctx, *inviteCode, *displayName, deviceName)
		}
		if err != nil {
			return err
		}
		return saveSession(normalizedURL, session)
	}

	if saved.Token == "" {
		return errors.New("not in a group yet, run create-group or join first")
	}
	switch command {
	case "set-server":
		normalizedURL, err := core.NormalizeServerURL(*serverURL)
		if err != nil {
			return err
		}
		moved := core.NewAPIClient(normalizedURL, saved.Token)
		resolvedURL, err := moved.ResolveBaseURL(ctx)
		if err != nil {
			return err
		}
		moved.BaseURL = resolvedURL
		me, err := moved.Me(ctx)
		if err != nil {
			return err
		}
		if me.Group.ID != saved.Group.ID {
			return errors.New("that server knows this PC as a member of a different group, nothing was changed")
		}
		return config.Update(func(stored *core.Config) { stored.ServerURL = resolvedURL })
	case "status":
		me, err := client.Me(ctx)
		if err != nil {
			return err
		}
		if me.Member.Status == proto.MemberPending {
			fmt.Printf("group %s: this PC is waiting for the owner to approve it\n", me.Group.Name)
			return nil
		}
		statuses, err := client.Worlds(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("group %s, you are %s\n", me.Group.Name, saved.Member.DisplayName)
		for _, member := range me.Members {
			fmt.Printf("  member %s  %-16s %s %s\n", member.ID, member.DisplayName, member.Status, member.DeviceName)
		}
		for _, status := range statuses {
			state := "free"
			if status.Lease != nil {
				state = "hosted by " + status.Lease.HolderName
				if status.World.JoinInfo != "" {
					state += " (join: " + status.World.JoinInfo + ")"
				}
			} else if status.SilentHost != nil {
				state = fmt.Sprintf("free, but %s was hosting and went silent at %s", status.SilentHost.HolderName, formatTime(max(status.SilentHost.RenewedAt, status.SilentHost.AcquiredAt)))
			}
			latest := "no save yet"
			if status.Head != nil {
				latest = fmt.Sprintf("last saved by %s at %s", status.Head.AuthorName, formatTime(status.Head.CreatedAt))
			}
			fmt.Printf("  world %-20s %-12s %s, %s\n", status.World.Name, status.World.GameName, state, latest)
		}
		return nil
	case "create-world":
		world, err := client.CreateWorld(ctx, proto.CreateWorldRequest{
			Name: *name, GameName: *gameName, DefaultSavePath: *savePath,
			DefaultLaunch: *launch, DefaultProcess: *process, DefaultInclude: *include,
		})
		if err != nil {
			return err
		}
		fmt.Printf("created world %s (%s), now run: peerly-cli configure -world %q\n", world.Name, world.ID, world.Name)
		return nil
	case "discard":
		return client.DiscardFork(ctx, *revisionID)
	case "keep", "unkeep":
		_, err := client.PinRevision(ctx, *revisionID, command == "keep")
		return err
	case "invite":
		invite, err := client.CreateInvite(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("invite code for one friend: %s (works once, expires %s)\n", invite.Code, formatTime(invite.ExpiresAt))
		return nil
	case "approve":
		return client.ApproveMember(ctx, *memberID)
	case "make-owner":
		return client.TransferOwnership(ctx, *memberID)
	case "remove-member":
		return client.RemoveMember(ctx, *memberID)
	}

	status, err := client.WorldStatus(ctx, *worldName)
	if err != nil {
		return err
	}
	world := status.World
	switch command {
	case "configure":
		resolved := core.ResolveSettings(world, config.World(world.ID))
		pinned := resolved.WorldSettings
		flags.Visit(func(given *flag.Flag) {
			switch given.Name {
			case "save-path":
				pinned.SavePath = *savePath
			case "launch":
				pinned.Launch = *launch
			case "process":
				pinned.Process = *process
			case "include":
				pinned.Include = *include
			case "checkpoint-minutes":
				pinned.CheckpointMinutes = *checkpointMinutes
			}
		})
		survey := core.SurveyFolder(core.ExpandPath(pinned.SavePath), core.ParseInclude(pinned.Include))
		printSurvey(survey)
		if survey.Problem != "" {
			return errors.New("settings were not saved")
		}
		if resolved.LaunchSuggestion != "" && pinned.Launch == "" {
			fmt.Printf("the group suggests this launch command, add it with -launch if you trust it: %s\n", resolved.LaunchSuggestion)
		}
		fmt.Printf("launch: %q  process: %q\n", pinned.Launch, pinned.Process)
		pinned.Confirmed = true
		return config.UpdateWorld(world.ID, func(settings *core.WorldSettings) {
			pinned.LastRevisionID, pinned.LastManifestHash = settings.LastRevisionID, settings.LastManifestHash
			pinned.LastFolder, pinned.LastInclude = settings.LastFolder, settings.LastInclude
			*settings = pinned
		})
	case "host", "sync":
		session := &core.Session{Client: client, Config: config, WorldID: world.ID, Timings: core.DefaultTimings(), SyncOnly: command == "sync",
			LocalChanges: core.LocalChangesPolicy(*localChanges)}
		session.OnEvent = func(event core.Event) {
			switch event.Kind {
			case core.EventPhase:
				return
			case core.EventProgress:
				if event.Total > 0 {
					fmt.Printf("\r%s %d%%   ", event.Message, event.Done*100/event.Total)
				} else if event.Message == "" {
					fmt.Print("\r")
				}
				return
			}
			fmt.Printf("\r[%s] %s\n", time.Now().Format("15:04:05"), event.Message)
			if event.Kind == core.EventLeased && *joinInfo != "" {
				if err := client.SetJoinInfo(ctx, world.ID, session.Lease().FencingToken, *joinInfo); err != nil {
					fmt.Println("could not publish join info:", err)
				}
			}
		}
		interrupts := make(chan os.Signal, 2)
		signal.Notify(interrupts, os.Interrupt)
		go func() {
			for range interrupts {
				session.Stop()
			}
		}()
		err := session.Host(context.Background())
		if lease, held := core.IsLeaseHeld(err); held {
			return fmt.Errorf("%s is hosting right now, join them in game %s", lease.HolderName, world.JoinInfo)
		}
		return err
	case "history":
		revisions, err := client.Revisions(ctx, world.ID)
		if err != nil {
			return err
		}
		for _, revision := range revisions {
			marker := " "
			if revision.ID == world.HeadRevisionID {
				marker = "*"
			}
			if revision.Pinned {
				marker += "K"
			}
			size := fmt.Sprintf("%8d KB", revision.Size/1024)
			if revision.PrunedAt != 0 {
				size = " (record)"
			}
			fmt.Printf("%s %s  %-28s %-10s %s  %s  %s\n", marker, revision.ID, revision.Branch, revision.AuthorName,
				size, formatTime(revision.CreatedAt), revision.Note)
		}
		return nil
	case "pull":
		target := *revisionID
		if target == "" {
			target = world.HeadRevisionID
		}
		if target == "" {
			return errors.New("this world has no save yet")
		}
		result, err := core.RestoreRevision(ctx, client, config, world, target, core.RestoreOptions{})
		if err != nil {
			return err
		}
		fmt.Printf("restored %s\n", target)
		if result.BackupDir != "" {
			fmt.Printf("the files that were here before are kept in %s\n", result.BackupDir)
		}
		for _, name := range result.Skipped {
			fmt.Printf("not written, outside your file filter: %s\n", name)
		}
		return nil
	case "promote":
		revision, err := client.Promote(ctx, world.ID, *revisionID)
		if err != nil {
			return err
		}
		fmt.Printf("main now points to %s\n", revision.ID)
		return nil
	case "delete-world":
		return client.DeleteWorld(ctx, world.ID)
	}
	fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("unknown command %q", command)
}

func formatTime(unixMillis int64) string {
	return time.UnixMilli(unixMillis).Format("2006-01-02 15:04")
}
