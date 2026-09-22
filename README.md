# peerly

Share one co-op game world with your friends when nobody wants to pay for a dedicated server.

Many survival and co-op games (Valheim, RuneScape: Dragonwilds, Enshrouded and others) keep the world on the PC of whoever hosts. If that person is away, nobody else can continue. peerly keeps the world file on a small server you run yourself, and hands it to whichever friend hosts next.

It does not replace the game's multiplayer. Friends still join the host through the game's own invite or join code. peerly only moves the save and decides who may host.

## How it decides which copy is the real one

The server holds the save history of each world and at most one **host lease** per world.

1. Pressing **Host** asks the server for the lease. The first person gets it, everyone else is told who is hosting and sees the join code the host shared.
2. The host's app downloads the latest save, swaps it into the game's save folder, and starts the game.
3. While the game runs the app renews the lease every 30 seconds. If the PC dies, the lease frees itself after 3 minutes.
4. When the game closes, the save is uploaded and the lease is released.

Only the lease holder can move the group's current save forward. Anything else, for example progress made offline or a host whose connection dropped long enough for someone else to take over, is stored as a **separate branch**. Nothing is overwritten and nothing is thrown away: the group opens History and chooses whether to make that branch the current world.

## Status

Early. The server, the sync logic and the interface are covered by automated tests, including a browser-driven end to end suite, all run on Linux.

The Windows window app (`peerly.exe`) compiles but has **not yet been run on a real Windows PC by the author**. `peerly-browser.exe` is the same app shown in your normal browser and is the safer choice until the window app has field reports. Please open an issue with what you find.

Game presets for Valheim and Dragonwilds are starting points. Every player checks the folder and the matched files on their own PC before the first sync, so a wrong preset is caught before it can touch anything.

## Set up the server

Any small Linux machine works, including the free ARM instances of Oracle Cloud. You need a domain name that points at it (a free DuckDNS name is fine) because the apps talk to the server over HTTPS.

```sh
make server-arm64            # or server-amd64
scp dist/peerly-server-linux-arm64 deploy/* you@server:
ssh you@server ./install.sh ./peerly-server-linux-arm64
```

The installer creates a `peerly` system user, a systemd service listening on `127.0.0.1:8787`, and prints the **admin key**. The admin key is needed once, to create a group.

Put Caddy in front for HTTPS: install Caddy, copy `deploy/Caddyfile` to `/etc/caddy/Caddyfile`, replace `saves.example.com`, and reload Caddy.

Oracle Cloud blocks ports 80 and 443 in two places, the VCN security list and the instance's own iptables. The installer prints the exact commands.

Data lives in `/var/lib/peerly` (one SQLite file plus one file per save). Back up that folder. The server keeps the 20 most recent session saves, 3 mid-session backups and 30 branch saves per world, and removes older ones.

## Use the app

Build the three Windows programs with `make windows` (they land in `dist/`).

- `peerly.exe`: the app in its own window (needs the WebView2 runtime, present on Windows 11 and most Windows 10 PCs)
- `peerly-browser.exe`: the same app in your browser
- `peerly-cli.exe`: command line version

Windows SmartScreen warns about programs it has not seen before, because the files are not code signed. Choose More info, then Run anyway, or build from source.

1. One person chooses **Create a group**, enters the server address and the admin key. They are the group owner.
2. The owner presses **Group and invites**, then **Invite a friend**, and sends that friend the text. Each invite code works once and expires after a day.
3. The friend chooses **Join a group** and pastes the server address and code. Their PC then waits until the owner presses **Approve** next to their name. The owner sees the name they typed and the name of their PC.
4. Whoever owns the world presses **Add world**, picks the game, and types the world name exactly as the save is named.
5. On every PC, the first **Host** or **Settings** shows which files in the save folder belong to the world. Only those files are ever uploaded, backed up or replaced. Characters and other worlds are left alone.
6. Press **Host** to play. Close the game when done and wait until the card says the world is free.

**Sync only** uploads progress made on this PC and downloads the latest save without starting the game.

### Things that go wrong with games, not with peerly

- **Steam Cloud** can put an old save back after peerly replaced it. Turn Steam Cloud off for the game, or for Valheim move the world to local storage first.
- Some games write the host's identity into the save (Palworld is the known case). Those need the save rewritten when another person hosts, which peerly does not do.
- A wrong process name means peerly cannot tell that you stopped playing. It keeps waiting and says so; press **Stop hosting** when you are done and the save is still uploaded. The right name is in Task Manager, Details tab, while the game runs.

## What it protects you from

- The save folder is checked before anything happens. Your home folder, Documents, Desktop, AppData roots, drive roots and system folders are refused, and so is any selection above 20,000 files.
- Replacing files is all or nothing. If the game or a cloud sync tool holds a file open, every file is put back and you are told which file was busy.
- Files on a PC that peerly has never synced are copied to a local backup folder and uploaded as a branch before they are replaced.
- Progress made outside a hosted session is never pushed over the group's world silently. You are asked, and if someone else hosted in between it always becomes a branch.
- The launch command suggested by whoever added a world is never run on your PC unless you accept it yourself. Only plain `steam://` and Epic launcher links are pre-filled.
- A save downloaded from the group can only write files that match your own file filter.
- The browser version accepts requests only from its own page, with a key that changes every run.

## What it does not protect you from

- A member you approved can download the group's worlds, host, and make any branch current. Approve only people you trust; the owner can remove a member at any time.
- The member token is stored in plain text in the app's settings folder, like most desktop apps store logins.
- Mid-session backups copy the save while the game is running. They wait until the files have been quiet for 20 seconds and are discarded if a file changes while being read, but a game can still be caught mid-write. If the latest save is such a backup, the next host is warned and can make an earlier save current from History. They can be turned off per world, per PC.
- One PC belongs to one group at a time. Leaving a group forgets it on that PC.

## Similar tools

- [SaveSync](https://www.savesync.games/) is a polished commercial app that stores saves through Steam Workshop. If you just want to play and do not care about self-hosting, it is the easy answer.
- [hoard](https://github.com/DevOfPie/hoard) and [dedicated-server-save-sync](https://github.com/Ayerdi/dedicated-server-save-sync) are self-hosted projects with a similar lease idea.
- [valheim-sync](https://github.com/RajaRakoto/valheim-sync), [palrelay](https://github.com/Lother13501350/palrelay) and [OpenSave](https://github.com/Liquid-co/OpenSave) cover single games or single-player multi-device sync.

## Development

Go 1.27 or newer. No other build tools; the interface is plain JavaScript embedded in the binary.

```sh
make test        # go vet + unit and integration tests with the race detector
make e2e         # browser-driven suite, needs python3 with playwright and Google Chrome
make all         # tests plus every binary into dist/
```

```
proto/          request and response types shared by server and client
server/         HTTP API, SQLite store (lease, revisions, retention), save files on disk
client/core/    sync logic: snapshot, transactional restore, host session, game detection
client/ui/      local web interface and its JSON API
client/web/     runs the interface in the browser
client/app/     runs the interface in a Wails window (Windows)
client/cli/     command line client
deploy/         systemd unit, Caddyfile, installer
e2e/            browser-driven end to end suite
```

Bug reports are most useful with `peerly.log` from the settings folder (`%AppData%\peerly` on Windows, `~/.config/peerly` on Linux).

## License

MIT
