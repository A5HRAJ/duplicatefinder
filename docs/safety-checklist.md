# File-safety checklist

Every way Duplicate Finder could lose, damage or wrongly move a file, and
whether a test proves it can't.

**Finished means every line below is ✅.** A bug found later means this
list was missing a line: add the line, add its test, fix it. Don't start a
new open-ended hunt.

| Mark | Meaning |
|---|---|
| ✅ | Proven by an automated test that runs on every release |
| 🟡 | Partly proven: only part of it is tested, or it was checked once by hand on the NAS, or it's only tested in a simulation where the real thing needs the NAS |
| ❌ | The app has a protection, but nothing proves it works |
| ❓ | Nobody has decided yet what the app should do |

*First pass, 2026-10-14.* The ✅ marks come from matching test names and
test steps to each line. Before anything else, check that each listed test
really proves what its sentence says, and downgrade the ones that don't.

**One thing to know first:** the app never deletes your files. It moves
them into a folder you pick, and you decide later whether to delete that
folder. The one exception is a move between disks with Verify on: the
original is deleted only once its copy has been checked at the other end
(section F).

---

## A. Deciding what counts as a duplicate

If two different files were wrongly called duplicates, moving "the spare"
would take away the only copy of something.

| # | The app must… | | Proof |
|---|---|---|---|
| A1 | Group two files only when every byte matches (a full fingerprint, never just size or name) | ✅ | `TestCandHashDiscriminates`, `TestWindowedScanMatchesWholeScan`. The fingerprint is 256-bit BLAKE3, so two different files matching by chance can be ignored. |
| A2 | Read every file again on every scan, never trust a fingerprint from an earlier scan | ✅ | `TestRescanRereadsEverything`, `TestResumeMatchesExactRequestOnly` |
| A3 | Not count two names for one file (hard links) as two copies, and never list names that are all one file as a duplicate group | ✅ | `TestHardLinksCountAsOneCopy`, `TestHardLinksAloneAreNotDuplicates`, smoke hard-link labels; checked on the DS124, 2026-11-17 |
| A4 | Not count one file reached by two routes (overlapping scan folders, shortcuts) as two copies | ✅ | `TestDirResolverCanonicalizesAliases`, `TestRefineGroupsNeverAliases` |
| A5 | Not count a NAS folder mounted back into the same NAS as a second copy | ✅ | `TestFindSelfMounts`, audit "mounted into itself" |
| A6 | Not follow shortcuts (symlinks) out of the folders you chose | ✅ | `TestDirHandleSurvivesPathSwap`, `TestOpenRelRefusesSwappedSymlink`, smoke symlink refusals |
| A7 | Never scan the recycle bin (`#recycle`) or snapshots (`#snapshot`). Otherwise a copy in the bin could count as "the copy that stays" and vanish when the bin is emptied | ✅ | Audit "recycle bin and snapshots": copies inside both are never listed or paired, and choosing one directly — as a scan folder, a reference folder, or through a shortcut — is refused (it was accepted until 2026-11-17); `TestACopyInTheRecycleBinNeverStays`: a bin copy in results saved before that is never the copy that stays, and the walk re-checks each root once pinned |
| A8 | Never merge different files when matching by name or created date | ✅ | `TestRefineGroupsByName`, `TestCreatedMatchUnknownNeverGroups` |
| A9 | Compare everything even in huge collections of same-size files, and report rather than silently skip anything over a limit | ✅ | `TestSkewedKeyStillFindsDuplicatesPastTheCap`, `TestKeyCounterDegradesWithoutFalseNegatives`, `TestOversizedGroupIsCappedAndReported` |
| A10 | Keep file names that aren't valid text (old Windows or Mac names) exactly as stored, and never mix them up with a similar-looking neighbour | ✅ | `bytesafe` tests; audit "names outside UTF-8" (runs on Linux in CI) |
| A11 | Never treat pipes, sockets or devices as files (opening one can freeze the app) | ✅ | `TestScannableIsRegularFilesAndDirectories`, `TestOpenRefusesFIFOWithoutBlocking`, audit "special files" |

## B. Always leaving one copy

| # | The app must… | | Proof |
|---|---|---|---|
| B1 | Hold one copy back when asked to move every copy in a group | ✅ | `TestKeepOneDrops`, smoke "whole duplicate group moves exactly one file" |
| B2 | Keep that rule across separate moves (the copy left by an earlier move is still protected) | ✅ | smoke "survivor of a drained group refused" |
| B3 | Keep that rule when copies are named through a different route | ✅ | `TestKeepOneDropsAlias`, smoke keep-one via alias |
| B4 | Never count a copy the app already moved as the copy that stays, even after its folder is renamed or the NAS restarts | ✅ | `TestKeepOneNeverLetsTheLastOriginalFollowItsMovedCopy`, `TestMovedCopyIsRecognisedByItsData`, audit "moved copies" (three sections) |
| B5 | With Verify on, re-read the copy that stays first, and refuse if it changed since the scan | ✅ | `TestKeepOneReadsTheCopyThatStays`, audit "a changed copy that would stay" |
| B6 | Count duplicates inside a folder being moved as moving too | ✅ | Audit "folders holding the last copy": two junk-only folders holding the same file never both leave — in one request, one after the other, or with a rescan between |
| B7 | Check which copies exist now, not when the scan ran (one deleted outside the app since doesn't count as staying) | ✅ | Audit "copies that went away after the scan": a copy deleted, or rewritten to another size, after the scan never counts as the copy that stays (verification off, so the existence check alone); `TestKeepOneDrops` for the decision itself |
| B8 | Never move anything out of a read-only reference folder, including through a shortcut, or by moving a folder that holds one | ✅ | `TestRefMatcherProtectsThroughAlias`, smoke reference refusals; audit "a reference folder inside a folder offered as empty": such a folder is not offered, and is refused when the reference was set after the scan (both happened until 2026-11-17) |
| B9 | Never move anything into a reference folder | ✅ | Audit "moves into a reference folder": refused into the folder, a folder inside it, and through a shortcut, in both layouts, with nothing created there |
| B10 | Warn you if the record of moved copies is lost or damaged, rather than quietly dropping the protection | ✅ | `TestLostRecordsWarnUntilAcknowledged`, `TestDamagedRecordsRecoveredFromResults`, audit "move records lost"; a folder record that cannot be read is listed as a scan issue naming its folder (`TestAnUnusableRecordIsReported`) |
| B11 | Keep the record of moved copies through an uninstall and reinstall | ✅ | Measured on the DS916+ (DSM 7.4), 2026-10-14: a throwaway package's data survived uninstall and reinstall. This is how DSM behaves, not app code, so one measurement is the proof. Re-measure after a major DSM upgrade |
| B12 | Behave safely when the record of moved copies is gone entirely (Package Center's Repair can reportedly erase it, or a move to a new NAS). The app can't tell that apart from a fresh install, so a copy it moved flat would look like an original again. Copies moved into a batch folder are still recognised by that folder | ✅ | Decided 2026-11-17: a flat move leaves a record in the folder the files landed in (`.duplicate-finder-moved`). `TestFlatMoveLeavesARecordInItsDestination`, `TestRecordedFilesAreRecognisedAndNothingElse`; audit "move records gone entirely": with the app's whole state deleted, the moved copy is still recognised and its original is refused, also after a restart. Checked on real DSM, 2026-11-17, on Btrfs (DS916+), ext4 and an exFAT USB disk (DS124): the record is written through File Station, survives a File Station copy of the folder, and identifies that copy on its own. A record speaks only for the folder it was written in: audit "a folder record copied beside the original" (copied there, it marked the original as moved until 2026-11-17, and with every copy marked, the last original could move). Since 2026-11-17 the record also lists the files inside a moved folder (audit "move records gone entirely" moves a folder), a big move writes several records, and what a scan recovers from them is saved at once (audit "protection recovered by a scan that is then stopped"). Protects flat moves made from 1.1.12 on |

## C. Moving only what you chose

| # | The app must… | | Proof |
|---|---|---|---|
| C1 | Move only files a scan actually listed | ✅ | smoke "move of a file no scan surfaced is refused" |
| C2 | Refuse a file that changed since its scan, even if its size and date look the same | ✅ | `TestMoveIdentityCatchesSameSizeSameMtimeRewrite`, smoke "changed since its scan" |
| C3 | Never move anything from the Conflicting Files list | ✅ | `TestMoveRefusesReadOnlyTool`, smoke read-only tool refusal |
| C4 | Move exactly what's ticked. Changing the ticks while the folder picker is open moves nothing | ✅ | smoke "swapping the selection while the chooser is open"; a move made from results another scan has since replaced moves nothing (`TestMoveFromReplacedResultsIsRefused`, smoke "a move says which scan its files were chosen from"; added 2026-11-17) |
| C5 | Never let Keep Newest or Select All tick a protected file or a copy the app already moved | ✅ | smoke "bulk select never selects protected rows", "Keep Newest never keeps or ticks a moved copy" |
| C6 | Refuse files File Station can't handle (`.DS_Store`, `._` files) and say why | ✅ | `TestFsCannotAddressMatchesMeasuredDSMBehaviour`, smoke "nomove" |
| C7 | Not move an "empty" folder that has gained a file since the scan, at any depth inside it | ✅ | `TestScanEmptyFoldersHardConfirm`, `TestTransitFolderThatGainedAFileIsKept`, `TestTransitNestedFolderThatGainedAFileIsKept`, audit "folders holding only empty folders"; `TestTransitRefusesAFolderThatGainedARealFileBeforeItsCopy`: with Verify on across disks, what is read for the copy is held to the same rule, so a file arriving after the check is not copied along (until 2026-11-17 it was) |
| C8 | Count a hidden folder (e.g. `.git`), a zero-byte file, or a folder named like junk as real content, at any depth inside a folder of empty folders | ✅ | `TestNativeJunkCheckRejectsJunkNamedDirectories`, `TestConfirmEmptyLooksInsideFolders`, `TestPassedOverIsContentMatchesTheConfirmation`, `TestJunkNamedFolderIsContentForItsParent`, `TestZeroByteFileIsNotJunk`, smoke "hiddenonly", audit "folders holding only empty folders" |
| C9 | Not call a folder empty just because it, or a folder inside it, couldn't be read, or held too much to read in full | ✅ | smoke "unreadable folder not reported as empty", `TestConfirmEmptyIsBoundedAndFailsClosed` |
| C10 | Never list backup files (`.bak` etc.) as junk, and refuse a temporary-file row that no longer looks like junk | ✅ | `TestBackupNamesAreNotJunk`, `TestMoveRefusesATempRowNoLongerJunk` |
| C11 | Let only NAS administrators move anything | ✅ | `test/gateway.sh` runs the real front door (`spk/ui/api.cgi`) against stand-ins for DSM's sign-in module and group lookup: no session, a non-administrator, a group merely named like administrators, a token that fails (never falling back to the cookie), a helper that fails after printing a name, and an empty, duplicated or smuggled token are all refused before the app is reached; `TestAuthMiddleware` for the daemon's own lock; the gateway hands the session on only to a listener owned by its own user (`TestTheSessionGoesOnlyToTheDaemonsOwnUser`; until 2026-11-17 to anything on the daemon's port) |
| C12 | Never run a scan and a move at once | ✅ | `TestScanRefusedWhileMoveActive`, `TestBeginMoveRefusesAScanThatStartedAfterThePrecheck` |
| C13 | Never run two moves at once | ✅ | Audit "a move cut short by a stop": while a move runs, the daemon refuses a second one at once and moves nothing; smoke for the window |
| C14 | Never let a file or folder name run as code in the administrator's DSM page — a script there could move or delete anything | ✅ | smoke "tooltips are TEXT": evidence, status and scope-list tooltips decode to text, never markup (the scope list's did not until 2026-11-17) |
| C15 | Never let a page on another site act with the administrator's DSM session, even with DSM's own cross-site protection turned off | ✅ | `TestStateChangesMustBeJSON`: the daemon accepts a request that changes anything only as JSON, which a browser will not send to another site without its consent (any content type was accepted until 2026-11-17) |

## D. Where files land

| # | The app must… | | Proof |
|---|---|---|---|
| D1 | Never overwrite a file already at the destination: a clashing name gets " (1)" | ✅ | `TestMoveViaFSCollisionStagesAndRenames`, `TestTransitNameClashLandsBesideTheExistingFile` |
| D2 | Refuse a destination outside the NAS's volumes (including through a shortcut) | ✅ | smoke "symlinked-out-of-volume dest rejected" |
| D3 | Refuse a destination inside the folder being moved | ✅ | `TestMoveRefusesADestinationInsideTheFolderBeingMoved`; audit "a folder moved into itself" (both layouts, nothing created) |
| D4 | Refuse moving a file into the folder it's already in | ✅ | smoke "plain move into the folder the file already sits in" |
| D5 | Give each batch its own folder, never mixing into an earlier batch's | ✅ | `TestAllocBatchFolder*`, smoke "second batch gets its own numbered folder" |
| D6 | When a name gets too long to add " (1)", leave the file in a clearly reported holding folder, never lose it | ✅ | Audit "a name too long for ' (1)'": a 255-byte name clashing at the destination ends in a holding folder the report names, verified intact, with the file already there untouched; `TestStagedMoveLostReplyParksInsteadOfDeleting` |
| D7 | Tell you where every file went, and keep that report until you've seen it, across a restart | ✅ | `TestMoveReportIsKeptUntilShownAndSurvivesARestart`, smoke kept report; a file put down under another name (" (1)", or the name a USB disk gives it) is named with where it is now (audit "a move under a new name is reported"; added 2026-11-17) |
| D8 | Refuse before starting if the destination doesn't have room | ✅ | audit "free space" |
| D9 | Put files on USB and external shares where you chose, even when the folder name differs from the share name | ✅ | `TestShareSpacePathUsesShareNamesNotDirectories`, smoke USB move |
| D10 | When the destination cannot store a file's name (FAT, exFAT and NTFS refuse characters such as `?`, `:` and `*`, and File Station silently swaps them for "-"), verify and record the copy under the name it really landed with, and never blame permissions for it | ✅ | `TestTransitFollowsANameTheDestinationChanged`, `TestUnverifiedMoveFollowsANameTheDestinationChanged`, `TestTransitFolderWithNamesTheDestinationChangesSaysSo`; the renaming measured on the DS124's exFAT USB disk, 2026-11-17 |

## E. Files arrive undamaged

| # | The app must… | | Proof |
|---|---|---|---|
| E1 | With Verify on, read the moved file back and compare it with the scan | ✅ | `TestMoveVerifyHappyPath`, `TestMoveVerifyCatchesTransitCorruption` and the other `TestMoveVerify*` tests |
| E2 | Between disks, delete the original only after its copy has been checked | ✅ | `TestTransitRemovesTheOriginalOnlyAfterTheCopyVerifies`; tested on the USB disk, 2026-10-11 |
| E3 | Leave the original untouched if the copy is damaged | ✅ | `TestTransitDamagedCopyLeavesTheOriginalUntouched`, smoke "damaged on its way to another volume" |
| E4 | Keep both if the original changes while being copied | ✅ | `TestTransitOriginalChangedAfterTheCopyIsKept`; `TestTransitKeepsANewerVersionWrittenInTheSameSecond` and its folder twin: a same-length rewrite within the same second, after the copy was verified, is caught by a nanosecond comparison (until 2026-11-17 the newer bytes were deleted) |
| E5 | Check moved folders file by file, at every depth | ✅ | `TestTransitFolderIsCheckedFileByFileThenRemoved`, `TestTransitDamagedFileInsideAFolderKeepsTheFolder`, `TestTransitFolderOfFoldersIsCheckedThenRemoved`, `TestTransitDamagedFileInANestedFolderKeepsTheFolder` |
| E6 | Refuse to move a file that has decayed since the scan | ✅ | `TestMoveVerifyRefusesDeepRotBeforeMoving`, `TestMoveVerifyRefusesUnreadableSource` |
| E7 | Never copy bytes for a move on the same disk (it's a rename) | ✅ | `TestVerifiedMoveOnOneFilesystemDoesNotCopy` |
| E8 | Keep Verify on by default and never turn it off by itself | ✅ | smoke move dialog |
| E9 | Never change a scanned file: a scan only reads | ✅ | Audit "a scan only reads": every entry on both volumes fingerprinted before and after a scan with all five tools — type, permissions, size, modified time, inode change time, link count, inode and content |

## F. The app's own deletes

| # | The app must… | | Proof |
|---|---|---|---|
| F1 | Remove its own temporary folders only when they're empty (DSM refuses otherwise) | ✅ | `TestStagingCleanupNeverDeletesAFileLeftBehind`; DSM's refusal measured on the NAS for 1.1.4 |
| F2 | Use its one "delete folder and everything in it" only on a copy proven spare | ✅ | `TestTransit*` |
| F3 | If stopped between hiding an original and deleting it, say where the hidden original is | 🟡 | Simulation only (`TestTransit*` checks the `.dupfinder-del-` name). Never tested with a real stop at that exact moment |
| F4 | Never overwrite an earlier report when exporting | ✅ | `TestExportViaFSCollisionRenames`, smoke "second export steps to a (1) name" |

## G. Interruptions

| # | The app must… | | Proof |
|---|---|---|---|
| G1 | When the package is stopped mid-move, keep the records and report what moved, what was in flight and what wasn't reached | ✅ | `TestReplay*`, audit "a move cut short by a stop"; checked on the NAS for 1.1.10 |
| G2 | Survive a power cut or crash mid-move with no warning: the same promise as G1, without the few seconds' grace | 🟡 | Each file's destination is written to disk before the file moves (`TestMoveRecordsWhereItWillLandBeforeFileStationMovesIt`, `TestJournalCutOffMidLineIsNotDamage`). Never tested with a real hard kill |
| G3 | Keep following a move when DSM's reply goes missing, never giving up on it or reporting a false failure | ✅ | `TestLostProgressReplyDoesNotAbandonARunningMove`, `TestLostStartReply*` |
| G4 | Lose nothing if a USB disk is unplugged mid-move | 🟡 | Safe by design (the original stays until its copy is checked). Never tested |
| G5 | Lose nothing if the destination fills up mid-move (something else writing to it) | ✅ | `TestDestinationFillingUpMidMoveLosesNothing`; audit "the destination fills up mid-move": verified or not, with "no space left" or a bare failure code, the original stays byte for byte, stays listed, is never taken for a moved copy, and the report says where any incomplete copy is. Until 2026-11-17 an unverified move could report the file "parked" and drop it from the list while the original never left. Since 2026-11-17 a cut-short copy is removed only under the file's own name or the name a USB disk gives it (`TestOnlyTheFilesOwnCutShortCopyIsRemoved`), and a lost answer while looking into the staging folder does not report the file as parked (`TestALostLookIntoStagingDoesNotCallTheFileParked`). File Station's own behaviour when a disk fills is simulated, not measured |
| G6 | Set aside a damaged results file instead of overwriting it, keeping the move records | ✅ | `TestDamagedResultsSetAsideRecordsKept`, `TestDamagedJournalWarnsAndIsKept` |
| G7 | Resume a stopped scan only with identical settings, never reusing old fingerprints otherwise | ✅ | `TestResumeMatchesExactRequestOnly`, release-gate resume checks |
| G8 | Tell you when results couldn't be saved | ✅ | smoke "a failed save is announced"; a failed save is never hidden by another warning shown after it (smoke "a move report says everything, once"; added 2026-11-17) |
| G9 | Keep the move records through a package upgrade | 🟡 | Checked by hand on the DS916+ at every release. No automated test |

## H. Hardware

| # | The app must… | | Proof |
|---|---|---|---|
| H1 | Pass all of the above on an Intel NAS | 🟡 | Installed and spot-checked on the DS916+ at every release. The automated tests run on the Mac and in CI, not on the NAS |
| H2 | …on a 64-bit ARM NAS (e.g. DS124) | 🟡 | The beta checklist walked on a DS124 (DSM 7.4.1), 2026-11-16 and 17: scans, verified moves, keep-one, reference folders, stops mid-scan and mid-move, hard links, an exFAT USB disk; the 1.1.12 changes (folder records, recycle-bin refusal, empty folders of folders) checked there by hand the same day. The automated tests run under emulation, not on the NAS |
| H3 | …on a 32-bit ARM NAS | 🟡 | Emulation only, and no hardware planned |
| H4 | …on both Btrfs and ext4 volumes | 🟡 | The DS916+'s main volume is Btrfs (copy-on-write shares); the DS124's is ext4, checked by hand there. USB disks checked by hand: ext4 on the DS916+, exFAT on the DS124 |

## I. Outside the app, but your files' fate

| # | Item | | Note |
|---|---|---|---|
| I1 | Moving a file out of a folder that Cloud Sync, Synology Drive, Hybrid Share or ShareSync keeps in step with other places removes it from those places too (the cloud account, other computers, another NAS). The app must ask before such a move, naming the folders and services, and move nothing unless you choose Move Anyway. A sync service it cannot check counts as "might be synced", so it asks then too | 🟡 | Decided 2026-10-14: warn. `TestSyncedFolders*`, `TestSyncHits*`, `TestDrive*`; smoke "a move out of a synced folder asks first", "Cancel moves nothing", "a service that did not answer still asks". Agreeing covers only the folders shown: one synced meanwhile is asked about (`TestSyncConsentCoversOnlyWhatWasShown`, smoke "a folder that became synced meanwhile is asked about"), and a move between two users' synced home folders is warned about (`TestSyncHitsSeesThroughTheTwoNamesOfAHome`); both added 2026-11-17. All of that runs against a simulated NAS. On the DS916+ only the shared-folder flags and an empty Cloud Sync list were seen. A real Cloud Sync task and Synology Drive have never been read from a real NAS |
| I2 | Suggest taking a snapshot before a large cleanup? | — | Decided 2026-10-14: no |

---

## Tally (2026-11-17)

| | Count |
|---|---|
| ✅ Proven | 66 |
| 🟡 Partly proven | 9 |
| ❌ Protected but unproven, or not yet protected | 0 |
| ❓ Undecided | 0 |
| Decided against | 1 (I2) |
| **Total** | **76** |
