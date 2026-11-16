Jitsi plugin for Mattermost with two additions on top of the upstream release:

* **Ring when someone else starts a meeting.** A ringtone in direct and group messages (until you open the conversation, click **Join Meeting**, or 30 seconds pass) and a short chime in channels. Silent in Do Not Disturb / Out of Office and in muted channels. Admins: *System Console → Plugins → Jitsi → Ring when a meeting starts*. Users: `/jitsi settings ring true|false`. Desktop and web apps only.
* **Clean meeting links.** The link in the meeting post no longer carries the viewer's username, so a link copied from the post and shared does not make everyone join under the copier's name. The name is still passed when you click the link.

**Install:** System Console → Plugins → Plugin Management → Upload Plugin, or `mmctl plugin add jitsi-*.tar.gz`. It replaces the official `jitsi` plugin and keeps its settings. To go back, upload the official release.

Check the download against `SHA256SUMS`.
