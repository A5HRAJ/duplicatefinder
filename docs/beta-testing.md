# Beta testing

Duplicate Finder has been exercised on one machine: a DS916+ (x86_64) running
DSM 7.4 on a btrfs volume. The ARM builds have only run under emulation.
Before the package is offered to everyone through SynoCommunity it needs to
be tried where the maintainer cannot try it, and this page says how.

## Is it safe to test on real data?

The app never deletes anything and never overwrites anything. The only write
it performs is a move, executed by DSM's own File Station with your account,
into a folder you pick; if you dislike the result, move the files back in
File Station. Still, for a first run:

- Scan a copy of some data, or a share you can afford to sort out by hand.
- Pick a destination folder on the same volume the first time, so a move is
  a rename rather than a copy.
- Leave **Verify file contents after moving** ticked.

## What needs testing

Anything in the right-hand column is untested. One report per cell is
enough to move it to the left.

| Area | Covered | Not yet covered |
| --- | --- | --- |
| Architecture | x86_64 | armv8 (DS218, DS220j, DS418...), armv7 (DS216j, DS416slim...) on real hardware |
| DSM version | 7.4 | 7.0, 7.1, 7.2, 7.3 |
| Volume filesystem | btrfs | ext4, and moves between a btrfs and an ext4 volume |
| Volumes | one | two or more volumes, with scan scope and destination on different volumes |
| External disks | none | a USB or eSATA disk as scan scope and as move destination (ext4, exFAT, NTFS) |
| Scale | tens of thousands of files | a volume with over a million files, or over ten million duplicate candidates |
| Accounts | one administrator | a second administrator using the app at the same time; DSM with CSRF protection turned off |
| Locale | English DSM | a DSM set to another language (the app's own text is English; DSM's widgets are not) |

## The checklist

Work through as much of this as your setup allows, and note anything that
surprised you, even if it is not a failure.

1. **Install.** Follow the README's install steps. Note whether the
   folder-permission step was clear, and what a scan says when a folder has
   not been granted yet.
2. **Scan every tool.** Add your folders under Scan Scope in the sidebar,
   leave all five tools ticked, and click Scan once: the scope is walked once
   and results appear tool by tool. Check that the counts in the left rail
   match what you would expect, and spot-check a few rows in File Station.
   Then untick some tools and scan again; only the ticked ones should change.
3. **Search and paging.** Use the search box, the magnifier menu (location,
   type, date range, size) and the column sort on a list longer than one
   page.
4. **Move, preserved.** Select a few duplicates, leave *Preserve original
   folder structure* selected, move them to a folder. Confirm a new
   `Duplicates` folder appeared inside it, mirroring the original paths, and
   that the moved rows disappeared from the list.
5. **Move, flat.** Repeat with *Move all files into same folder*, including
   two files with the same name, and confirm the second got a ` (1)` suffix
   rather than replacing the first.
6. **Move across volumes or onto an external disk**, with verification on.
   This is the path that copies data, so it is the one that matters most on
   ext4 and USB.
7. **Keep-one.** Try to select every copy in a duplicate group; the last one
   must refuse. Move all but one, then confirm a rescan shows the group gone.
8. **Reference folders.** Add a read-only reference folder in the sidebar
   and confirm, without rescanning, that its files show a padlock and cannot
   be selected or moved, and that moving into that folder is refused.
9. **Interrupt a scan.** Start a long duplicates scan, then stop the package
   in Package Center and start it again. Reopen the app: it should say the
   scan was interrupted, and the next Scan click should offer Resume and
   Start Over.
10. **Conflicting Files.** If any sets appear, read the Status and Evidence
    columns for a few and judge whether the wording makes sense. Export the
    list as CSV and open it.
11. **Upgrade in place.** When a newer package version is available,
    install it over the old one and confirm the results and settings
    survived.
12. **Hard links.** If a share holds hard links (backups made with
    `rsync --link-dest` do), confirm the rows are labelled "hard link" and
    that the group header and the reclaimable total count the file once.

## Reporting

Open an issue with the **Bug report** template for anything that went wrong,
and a plain issue titled "Beta report: <model>, DSM <version>, <filesystem>"
for a run that went well, listing the checklist items you covered. The
daemon's log is at `/var/packages/<package id>/var/dupfinder.log` on the NAS
(readable as root over SSH); its lines name your own files, so redact what
you do not want public.

## Known gaps going in

- The interface is English only.
- Created dates come from File Station and are blank where it has none.
- The emulated ARM test suite once showed an intermittent failure while
  moving a junk-only folder; it did not reproduce in 18 consecutive runs
  afterwards. A junk-only folder move that fails on real ARM hardware is
  exactly the report to send.
