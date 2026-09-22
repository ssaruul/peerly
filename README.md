# peerly

**Play your co-op world with friends even when the usual host is not around.**

In games like Valheim, RuneScape: Dragonwilds or Enshrouded, the world lives on the computer of whoever hosts. If that friend is busy tonight, nobody else can continue the world. Renting a dedicated server fixes this, but it costs money every month for something a group of four uses a few evenings a week.

peerly is a small free program that keeps the group's world in one shared place and hands it to whoever wants to host next. Whoever is free presses **Host**, the newest version of the world lands on their PC, they play with the others, and when they close the game the world is saved back for everyone.

peerly does not change how the game does multiplayer. Your friends still join you inside the game as usual. peerly only makes sure the right save file is on the right PC, and that two people never play two different versions of the same world.

## What is the catch?

Someone in the group needs to run a tiny server. That can be almost any always-on Linux machine, including the free tier at Oracle Cloud, so the money cost can be zero. Setting it up takes one person an hour and a willingness to paste some commands into a terminal. Everyone else only installs the app.

If nobody in your group wants to do that, [SaveSync](https://www.savesync.games/) on Steam does a similar job for a few dollars per person and needs no server.

## Status: early

peerly works in automated tests on Linux, including a test suite that clicks through the app in a real browser. It has **not yet been used on a real Windows PC with a real game by the author**. If you try it, please open an issue and say what happened, good or bad. Until then, use it with a world you have backed up yourself.

## How it works, in plain words

Think of the world as a library book. The server holds the book, and there is one **host lease**, which is the right to hold the book right now.

1. You press **Host**. If nobody has the lease, you get it. If a friend already has it, you are told so and shown the code to join their game.
2. Your app downloads the newest save and puts it where the game expects it. The files that were there before are kept in a backup folder.
3. The game starts. While it runs, your app pings the server every 30 seconds to say "still here". The ping carries no save data. If your PC dies, the pings stop and after 3 minutes the server frees the world for the others.
4. Games write the world to disk only when they save (their own autosave, a manual save, or closing). Whenever the game has saved, and the files have then stayed unchanged for 20 seconds so the game is not caught mid-write, peerly uploads that save as a mid-session backup, at most once every 15 minutes (you can change or disable this). What happens in the game between saves is only in memory and cannot be backed up by anything.
5. When you close the game, the save is uploaded and the lease is released. The next host gets exactly what you left.

**What if something goes wrong?** Nothing is ever overwritten silently. If someone played a version of the world that is not the current one, for example because they played offline or their connection dropped and a friend took over, that version is kept as a **separate branch**. The same happens to a save that is less than half the size of the current world, because a world that suddenly shrinks usually means the game started a fresh world under the old name or could not load the old one. The group opens **History** and decides which version should be the current world. Older versions stay in History too, so a bad decision can be undone.

## For players: setting up the app

You need Windows 10 or 11.

1. Get `peerly.exe` (or `peerly-browser.exe`, which shows the same app in your web browser) from the person in your group who runs the server, or build it yourself, see below. Windows will warn that the program is unknown because it is not signed; choose *More info*, then *Run anyway*.
2. Ask the group owner for an **invite**. It contains the server address and an 8-character code. Each code works once and for one day.
3. Open peerly, choose **Join a group**, paste the address and the code, type your name.
4. Your screen says *Waiting for the group owner*. The owner sees your name and your PC's name and presses **Approve**. The screen updates by itself.
5. The first time you press **Host** or **Settings** on a world, peerly shows which files in the game's save folder belong to that world. Check the list, press *Looks right, continue*. Only those files are ever touched. Your characters and other worlds are left alone.

From then on: press **Host** to play, close the game when done, wait until the world shows as free. **Sync only** uploads what you have and downloads the newest save without starting the game.

### Questions players ask

**Do all my friends need peerly?** Only those who want to host. Friends who only ever join someone else's game do not need it. Their progress is inside the host's world file, which the host's peerly uploads.

**Do I have to find my save files myself?** Usually not. The person who adds the world picks the game from a list, and the folder is pre-filled for everyone. You only confirm that the files shown are the right ones. The entries for Valheim and RuneScape: Dragonwilds were checked against public documentation, not yet on a real PC.

**What if two of us press Host at the same time?** The server gives the lease to the first request. The other person is told who is hosting.

**What if my PC crashes while hosting?** The world is freed after 3 minutes. The group continues from the last mid-session backup, which holds the game's last autosave before the crash (or the one before it, if the crash came within 15 minutes of the previous backup). Everything after the game's last autosave is lost, as it would be without peerly. When you are back, peerly uploads what is on your disk as a separate branch so nothing that was saved is lost.

**What if my internet drops for a while during a session?** peerly keeps trying to ping and to upload in the background and tells you so. A drop shorter than about 3 minutes changes nothing. A longer one frees the world; if no friend took it in the meantime, your session simply continues on the current world, and if someone did, your progress becomes a separate branch.

**Can peerly delete my save?** It replaces the world files only after copying them to its backup folder (shown in Settings), every time, and only the files that match the world's filter. The five most recent backups are kept. Files it does not recognise are never touched.

**What can go wrong that peerly cannot fix?** Steam Cloud may put an old save back after peerly replaced it, so turn Steam Cloud off for the game (in Valheim, move the world to local storage). A few games write the host's identity inside the save (Palworld is the known case); those need more than a file copy and are not supported.

## For the friend who runs the server

You need a Linux machine that is always on and reachable from the internet, and a domain name that points at it. A free name from DuckDNS is fine. The free ARM instance on Oracle Cloud works and costs nothing.

You do not need to understand the server. You need to do these steps once.

1. On your own PC, build the server program with `make server-arm64` (for Oracle's ARM machines) or `make server-amd64`. This needs Go installed. The file lands in `dist/`.
2. Copy that file and the `deploy/` folder to the machine, then run `./install.sh ./peerly-server-linux-arm64` there. This creates a system user, starts the service, makes it start again after reboots, and prints an **admin key**. Keep that key; it is only needed to create a group and to rescue a group whose owner has vanished.
3. Install [Caddy](https://caddyserver.com/), copy `deploy/Caddyfile` to `/etc/caddy/Caddyfile`, replace `saves.example.com` with your domain name, and reload Caddy. Caddy gives your server HTTPS automatically.
4. On Oracle Cloud, open ports 80 and 443 in two places: the *security list* of your network in the web console, and the machine's own firewall. `install.sh` prints the exact commands.
5. In peerly on your PC choose **Create a group**, enter your domain and the admin key. You are now the group owner. Invite friends from **Group and invites**.

The server has exactly one setting, the admin key. `install.sh` generates it into `/etc/peerly/env`. If you install by hand instead, copy `deploy/env.example` to `/etc/peerly/env` and fill in a long random value.

Your data lives in `/var/lib/peerly`: one small database and one file per save. To make a backup while the server runs:

```sh
sudo install -d -o peerly -g peerly /var/backups/peerly
sudo -u peerly /usr/local/bin/peerly-server -data /var/lib/peerly -backup /var/backups/peerly
```

Copy the dated folder somewhere safe. Restoring is copying it back to `/var/lib/peerly` and restarting the service.

The server keeps, per world, the latest session save of each of the 5 most recent hosts, 3 mid-session backups, and every branch save younger than 30 days (plus the 30 newest older ones). One person's sessions replace each other, so however often one friend hosts, the last save of everyone before them stays until they host again. Five people with a 300 MB world is 1.5 GB per world. History still lists every session for six months after its file was removed, so the group can always see who hosted when. A PC whose last synced copy is no longer on the server uploads that copy as a branch before replacing it.

**Upgrading**: replace the binary and restart the service. The database is upgraded automatically. A database written by a newer server is refused rather than damaged.

**Moving to a new domain**: copy `/var/lib/peerly` to the new machine and point the domain at it. Every player then uses *Change server address* at the bottom of the app; their membership carries over.

**If the owner disappears** (lost PC, left the group without handing over), nobody can invite or approve any more. Whoever has the admin key fixes that from any PC:

```sh
peerly-cli admin-groups  -server https://your.domain -admin-key KEY
peerly-cli admin-recover -server https://your.domain -admin-key KEY -group GROUP_ID -as yourname
```

Owners avoid this by handing the group over first: **Group and invites**, **Make owner**.

## What it protects you from

- Only the files you confirmed are ever uploaded, backed up or replaced.
- The save folder is checked first. Your home folder, Documents, Desktop, drive roots and system folders are refused, and so is anything with more than 20,000 files.
- Replacing files is all or nothing. If the game or a cloud sync tool holds a file open, every file is put back and you are told which one was busy.
- Files on a PC that peerly has never seen are backed up locally and uploaded as a branch before they are replaced.
- Progress made outside a hosted session is never pushed over the group's world silently. You are asked, and if someone else hosted in between it always becomes a branch.
- A launch command suggested by whoever added the world never runs on your PC unless you accept it yourself.
- Joining needs an invite that works once, and the owner's approval. Invite codes cannot be guessed because attempts are limited.
- One PC runs one copy of peerly, and the server does not let the same person host the same world from two places at once. After a crash, the restarted app takes over by itself a few minutes later.

## What it does not protect you from

- A member the owner approved can download the worlds, host, and make any branch current. Approve people you trust.
- The login token is stored in plain text in the app's settings folder, like most desktop apps store logins.
- Mid-session backups copy the save while the game runs. peerly waits for a quiet moment and discards a file that changes while being read, but a game can still be caught mid-write. If the newest save is such a backup, the next host is warned and can make an earlier save current from History.
- One PC belongs to one group at a time.

## Similar tools

- [SaveSync](https://www.savesync.games/): commercial, stores saves through Steam Workshop, no server needed. The easy answer if you do not want to self-host.
- [hoard](https://github.com/DevOfPie/hoard) and [dedicated-server-save-sync](https://github.com/Ayerdi/dedicated-server-save-sync): self-hosted projects with a similar lease idea.
- [valheim-sync](https://github.com/RajaRakoto/valheim-sync), [palrelay](https://github.com/Lother13501350/palrelay), [OpenSave](https://github.com/Liquid-co/OpenSave): one game each, or one player across several devices.

## For developers

Go 1.27 or newer, nothing else. The interface is plain JavaScript embedded in the binary.

```sh
make test        # go vet + unit and integration tests with the race detector
make e2e         # browser-driven suite, needs python3 with playwright and Google Chrome
make windows     # peerly.exe, peerly-browser.exe, peerly-cli.exe into dist/
make all         # tests plus every binary
```

```
proto/          request and response types shared by server and client
server/         HTTP API, SQLite store (lease, revisions, retention, migrations), save files on disk
client/core/    sync logic: snapshot, transactional restore, host session, game detection
client/ui/      local web interface and its JSON API
client/web/     runs the interface in the browser
client/app/     runs the interface in a Wails window (Windows)
client/cli/     command line client
deploy/         systemd unit, Caddyfile, installer, env.example
e2e/            browser-driven end to end suite
```

Environment variables: the server reads `PEERLY_ADMIN_KEY` (see `deploy/env.example`); the apps read the optional `PEERLY_CONFIG_DIR` to use a different settings folder, which the tests use to run several players on one machine.

Bug reports are most useful with `peerly.log` from the settings folder (`%AppData%\peerly` on Windows, `~/.config/peerly` on Linux).

## License

MIT
