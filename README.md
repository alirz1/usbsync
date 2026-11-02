# USB Sync for DSM 7 (x86_64)

Two-pane rsync tool for Synology NAS (built for SA6400 / epyc7002, works on any x86_64 DSM 7.x).
Left pane: NAS shared folders. Right pane: attached USB/eSATA drive(s). Copy NAS → USB or USB → NAS.

## Install
1. Package Center › Settings › General › Trust level: "Any publisher".
2. Package Center › Manual Install › choose `USBSync-1.0.0-0001-x86_64.spk`.
3. Grant access: Control Panel › Shared Folder › edit each share you want to sync (and `usbshare1` etc.)
   › Permissions › "System internal user" › `USBSync` › Read/Write.
4. Open "USB Sync" from the DSM main menu (or http://<nas>:8765) and sign in with a DSM admin account.

## Notes
- Runs as the unprivileged `USBSync` package user (DSM 7 rule), so ownership/ACLs are not copied;
  files get normal permissions. On ext4/btrfs drives permissions + symlinks are kept; on exFAT/NTFS/FAT
  a 2-second timestamp window is used.
- Selected folders are copied *as folders* into the destination (dest/photo, dest/music …).
- "Mirror" adds `--delete`, scoped to those copied folders only.
- Drives are tracked by filesystem UUID, so jobs still find a drive if it remounts as usbshare2.
- Port override: create `/var/packages/USBSync/var/usbsync.env` with `PORT=9000` (also update the firewall rule).
- Data, jobs, history and logs: `/var/packages/USBSync/var/`.

## Build
`./build.sh` (needs Go 1.22+) → `dist/USBSync-<version>-x86_64.spk`
