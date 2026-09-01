# The phone remote

The built-in web remote: the live stream, the songs you have saved,
the accounts that reach them, and the car. See the
[README](../README.md) for the short version and the setup steps.

## Listening from your phone

Infinite AI Radio has a built-in web remote: a phone-first page with the
live stream, the now-playing state and the shared steering context (the
base sound plus every accumulated tweak, identical for every listener
and after every reload), a steering box with a lyric-writer switch, a
start-fresh action, a Next button, a session picker (save the current session under a name, load
any session or preset), save buttons with a tag field at the top of the
page, and a player for the tracks you have saved. Everything on it
needs a login, and the first account is created in the terminal:

```sh
./iar remote setup          # create the first admin (email + password, typed twice)
./iar --remote              # start as the station: remote on, local speakers at 0
```

With `--remote` the local speakers start at volume 0 (the machine is
the station, not the listening room); type `volume 80` in its terminal
to also hear it locally. Remote listeners always receive the
full-level stream either way.

(`remote.enabled` in the config keeps it on permanently.) The player
prints the URL to open; `iar doctor` shows it too under the remote
section. Open it on the phone, log in, tap play. The stream is MP3 at
~192 kbps and runs a few seconds behind the machine's speakers;
steering, starting fresh and saving act instantly and show up in the
terminal as well. Every button disables itself while its request is in
flight and reports success or failure right next to itself.

The connection looks after itself: if the stream drops or stalls (weak
signal, switching networks, the machine rebooting), the page
reconnects on its own with growing pauses, retries the instant
connectivity returns, and picks up the moment the stream is reachable
again; only an expired login stops it, with a message saying to log in
again. On phones the page defaults to **buffered playback**: it
downloads whole upcoming tracks ahead of time and plays them
back-to-back, so the music keeps going through minutes of dead signal
and steering still switches to the new sound as soon as its first
track is downloaded. A switch under **Settings** chooses between
buffered and the direct live stream; the direct stream is what
non-browser players (VLC, `mpv`) get from `/stream.mp3`.

How much is buffered is a per-device choice next to that switch,
with the banked minutes shown beside it:

- **Economical** downloads one track ahead and keeps only a few,
  for metered connections.
- **Automatic** (the default) downloads about two ahead on cellular or
  with data saving on, more on Wi-Fi, and keeps roughly 15-20 minutes.
- **Maximum** fills the device with about 45 minutes of audio
  (roughly 60-70 MB at the stream's quality) so long dead zones and
  flights stay covered.

In buffered mode the Next button skips only on that device: the
machine's speakers and other listeners keep their own position. Use
the direct stream's Next to skip the shared stream for everyone.

Saving from the phone always captures what YOU are hearing: in
buffered mode that is this device's playing track, which may trail the
machine's speakers. Save is the left button in the bottom bar; its
heart fills once that track is in your snippets, and saving it again
does nothing. Saves are filed under the tag set in Settings, where
"Save the previous track" also lives. Every
track carries a generated short title (an evocative two-to-four word
name) and a genre/mood line, which is what lock screens, saved-song
lists and car displays show instead of the raw prompt, along with a
"Track N" counter.

The page also publishes media-session metadata, so the phone's lock
screen, Bluetooth displays and car interfaces show what is playing
(short title, track number and genre line, artwork) with working play,
pause and next buttons. The remote can be installed as an app from the
browser menu ("Add to Home screen"); how much of its identity a car
display shows depends on the browser and is outside the page's
control.

### The layout

One screen, no page scrolling. The top bar holds a signal light, the
Live/Saved switch and Settings. The bottom bar holds Save, Play and
Skip, and never moves. Between them: what is playing - including the
language it is being sung in - the language switches when you have
configured any, and the station list - presets and your saved sessions
- where one tap on a row starts it. Steering and the current prompt sit
in a section you open when you want them; the words of the playing
track sit in a section below it that is open to begin with, since they
change with every track. Everything set once per device - buffered
playback, the car conveniences, the save tag, the lyric writer, the
language list - lives under Settings.

### Starting the audio

Browsers refuse to make sound on a page that has not been touched yet,
so a reload while listening cannot resume by itself the first time.
The page says "tap anywhere to start the audio" and the next tap
starts it - no need to find the play button. To skip that tap for
good, add the page to your home screen: an installed web app is
allowed to start audio on its own. That exemption needs the remote to
be served over HTTPS, so on a plain `http://` tailnet address the
one-tap start is the way it works. Firefox for Android also has a
per-site setting (Settings > Site settings > Autoplay > "Allow audio
and video"); Chrome and Opera for Android have no such setting.

Two conveniences are built for the car, both switchable under Settings
and remembered per device:

- **The previous-track button saves the track.** Car displays only
  offer the standard media buttons, and a web page cannot add a
  labelled "save" to them (the only way to get one would be a small
  native companion app). Since an endless generated stream has no
  meaningful "previous track", that button doubles as
  save-what-I-am-hearing: press it on the steering wheel, headset or
  car screen and the current track lands in your snippets, confirmed
  by a short "Saved:" flash in the track title. Be aware it captures
  every previous-track input, including a voice assistant's "previous
  song", and the button keeps its standard icon. Turning the toggle
  off removes the button from the car instead of leaving a dead one.
  In the saved-songs player, previous keeps its normal meaning.
- **Resume when the car reconnects.** When the car turns off (or the
  Bluetooth route drops), playback pauses and the page keeps the
  paused stream and its media notification alive. It resumes by itself
  when the car asks to play, when you open the page, or with one tap
  otherwise, and never on a timer, so a phone in a pocket stays
  silent. For hands-free resume, enable "Automatically resume media"
  in Android Auto's settings. Reloading the page while you were
  listening also picks playback straight back up.

For safety the remote binds only to localhost and, when the machine has
one, its Tailscale address; it never listens on your LAN or the internet
unless you add addresses to `remote.bind` in the config. The intended
setup is a private [Tailscale](https://tailscale.com) network between
your computer and phone:

1. Install Tailscale on the computer per the official Linux guide
   (`https://tailscale.com/download/linux`; installing and running
   `sudo tailscale up` needs sudo) and sign in.
2. Install the Tailscale app on the phone and sign in to the same
   account.
3. Run `./iar remote setup` once, then start `./iar --remote`.
4. Open the printed `http://100.x.y.z:8246` URL in the phone's browser,
   log in and tap play.

To also reach the remote on your home LAN (say the machine is
192.168.1.20), add that address to `remote.bind`; the player keeps
listening on localhost and the tailnet as well, and addresses you bind
are automatically accepted in URLs:

```json
{ "remote": { "enabled": true, "bind": ["192.168.1.20"] } }
```

Then open `http://192.168.1.20:8246/` from any device on that network
and log in. `"bind": ["0.0.0.0"]` listens on every network the machine
is on.

Security notes, plainly:

- Logins happen over plain HTTP. Tailscale encrypts everything between
  the devices, so that is fine on the tailnet. On your home LAN a
  password travels in clear text across your LAN; that is your call for
  a network you trust. Never expose the port to the public internet
  without TLS in front of it (a reverse proxy with a certificate);
  behind such a proxy the login cookie is marked secure automatically.
- A login lasts 30 days of inactivity on that browser (it is a radio).
  Log out from the menu to end it early; changing or resetting a
  password logs every other device out.
- Failed logins are rate-limited per address and per account, and the
  page never reveals whether an email exists.
- The remote refuses requests whose Host or Origin is not localhost,
  your tailnet address, an address you bound, or an entry in
  `remote.allowed_hosts` (a cross-site and DNS-rebinding defense).

## Accounts and permissions

Every listener has an account: the email address is the login, and
only the account holder ever knows the password. The first admin is
made with `./iar remote setup`; after that everything happens on the
remote's **Users** page (visible to admins):

- **Adding a user** takes an email and four checkboxes. It produces an
  invitation link, shown with a Copy button (and emailed too if
  [email is set up](#email-setup)). Send it by text or chat; the person
  opens it, sees their email, chooses a password, and is logged in. The
  link works once and expires after 7 days. The invitee has to be able
  to reach the address in the link, so create it from a browser that is
  on the same route in (the tailnet address for tailnet users, the LAN
  address for LAN users).
- **Pending links** are listed with Regenerate (which invalidates the
  old link) and Revoke.
- **Permissions** are four independent switches per account: *admin*
  (manage users and links, delete sessions, delete saved songs, edit the
  vocal-language list), *can steer* (steer, skip, and switch a configured
  language on or off), *new prompts* (start a prompt, and load a session
  or preset, since both change what everyone hears), *can save* (save
  tracks, rename and regroup saved songs, and save the current session
  under a name). Listening needs none of them. Admin does not imply the
  other three; an admin can tick them for themselves. The page only
  shows the controls an account may use, and the server refuses the
  rest either way.
- **Password reset**: an admin presses *Reset link* on the account. The
  link works once, expires after 24 hours, and the user chooses the new
  password themselves; every other login of that account ends.
- **Guard rails**: you cannot delete your own account, and the last
  admin can neither be deleted nor demoted.

Each user changes their own password on the **Account** page. Forgot it?
An admin hands you a reset link. If the only admin is locked out, run
`./iar remote setup` again with that email in the terminal: it resets the
password and restores every permission.

Accounts and login sessions are small JSON files under the data
directory (`remote/users.json`, `remote/sessions.json`), readable only
by your user. Logins, user changes and refused actions all show up in
the log.

## Email setup

Optional. With no email configured, you pass invitation and reset links
on yourself, and nothing is missing. If you would rather have them
emailed automatically as well, point `remote.smtp` at a mail provider:

```json
{ "remote": { "smtp": {
    "host": "smtp-relay.brevo.com", "port": 587, "tls": "starttls",
    "username": "your-login", "password": "your-smtp-key",
    "from": "radio@example.com" } } }
```

and check it with:

```sh
./iar remote test-email you@example.com
```

Providers with a free tier that works for this (re-checked August
2026; numbers change, so confirm on their pricing pages):

- [Brevo](https://www.brevo.com): 300 emails a day on the free plan,
  SMTP relay at `smtp-relay.brevo.com:587`.
- [Resend](https://resend.com): 3,000 emails a month (100 a day) free,
  SMTP at `smtp.resend.com:465` with `"tls": "tls"` (username
  `resend`, password is an API key).
- [Mailjet](https://www.mailjet.com): 200 a day (6,000 a month) free,
  SMTP relay at `in-v3.mailjet.com:587`.
- Gmail: `smtp.gmail.com:587` with an
  [app password](https://support.google.com/accounts/answer/185833)
  (requires 2-step verification on the Google account); about 500
  messages a day.

`"tls"` is `"starttls"` (default, port 587), `"tls"` (implicit TLS, port
465) or `"none"`. Keep the config file readable only by you when it
holds a password (`chmod 600`); `iar doctor` warns otherwise.

There is deliberately no "just send it from my computer" mode: mail sent
straight from a home connection is blocked by most ISPs and rejected or
spam-foldered by the big mailbox providers, so it would fail quietly
for exactly the people it is meant for. The links are the reliable path;
email is a convenience on top.

## Saved songs and tags

The remote's **Saved** mode plays the songs you have saved, entirely on
the phone: it never touches the live stream, other listeners, or the
machine's speakers.

- Tick the tags and the languages you want (every tag and every sung
  language with at least one saved song is listed - songs from before
  languages were recorded show as *unknown*; all are selected to begin
  with) and the player loops through the checked songs forever.
- Every song row carries a checkbox: uncheck a song to skip it in the
  loop without deleting anything. *Check all* and *Uncheck all* work on
  the songs currently in view, and *Move checked…* regroups them under
  any tag - typing a new name creates a new group on the spot.
- A song row shows its title (full, wrapped - never cut off), genre
  line, language, tag, length and when it was saved, with a *Play* and
  a *Loop this one* button. Tapping the row itself opens the song's
  panel: the full lyrics, its details and file name, and buttons to
  *Download* the MP3 to this device, *Rename* it, *Move* it to another
  tag, or *Delete* it (with a confirmation; the file and its lyrics are
  removed from the server). Looping one song repeats it until you press
  *Back to looping the checked songs*.
- Switching between the Live and Saved tabs is just looking: whatever
  is playing keeps playing, and the bottom controls follow the playing
  side until you actually start the other one.
- The built-in controls seek, pause and set volume as usual.
- The mode, tag, language and checkbox selections are remembered per
  browser.

Saving from the phone works like the terminal `save`: an optional tag
in the box next to the button, the song appears in the list a moment
later. On disk, a saved song's file is named after its title - in the
title's own script - with the sung language's tag before the extension
(`20260101-120000-tumutunaw-ang-selyo.tl.mp3`), and the full lyrics sit
next to the MP3 in a `.txt` with the same base name, so what you see in
the interface is what you can find in the folder.
