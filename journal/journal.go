package journal

import (
	"fmt"
	"io"
	"os"
)

// Example of journal records written for 2004.1

// @pv@ 0 @db.depot@ @import@ 0 @subdir@ @import/...@
// @pv@ 3 @db.domain@ @import@ 100 @@ @@ @@ @@ @svn-user@ 0 0 0 1 @Created by svn-user@
// @pv@ 3 @db.user@ @svn-user@ @svn-user@@svn-client@ @@ 0 0 @svn-user@ @@ 0 @@ 0
// @pv@ 0 @db.view@ @svn-client@ 0 0 @//svn-client/...@ @//import/...@
// @pv@ 3 @db.domain@ @svn-client@ 99 @@ @/ws@ @@ @@ @svn-user@ 0 0 0 1 @Created by svn-user@
// @pv@ 0 @db.desc@ 1 @add@
// @pv@ 0 @db.change@ 1 1 @svn-client@ @pallen@ 1363872228 1 @add@
// @pv@ 3 @db.rev@ @//import/trunk/src/file.txt@ 1 1 0 1 1363872228 1363872228 00000000000000000000000000000000 @//import/trunk/src/file.txt@ @1.1@ 1
// @pv@ 0 @db.revcx@ 1 @//import/trunk/src/file.txt@ 1 0
// @pv@ 0 @db.desc@ 2 @copy@
// @pv@ 0 @db.change@ 2 2 @svn-client@ @pallen@ 1363872334 1 @copy@
// @pv@ 0 @db.integed@ @//import/branches/copy/src/file.txt@ @//import/trunk/src/file.txt@ 0 1 0 1 2 2
// @pv@ 0 @db.integed@ @//import/trunk/src/file.txt@ @//import/branches/copy/src/file.txt@ 0 1 0 1 3 2
// @pv@ 3 @db.rev@ @//import/branches/copy/src/file.txt@ 1 1 3 2 1363872334 1363872334 00000000000000000000000000000000 @//import/trunk/src/file.txt@ @1.1@ 1
// @pv@ 0 @db.revcx@ 2 @//import/branches/copy/src/file.txt@ 1 3
// @pv@ 0 @db.integed@ @//import/branches/merge/src/file.txt@ @//import/trunk/src/file.txt@ 0 1 0 1 2 2
// @pv@ 0 @db.integed@ @//import/trunk/src/file.txt@ @//import/branches/merge/src/file.txt@ 0 1 0 1 3 2
// @pv@ 3 @db.rev@ @//import/branches/merge/src/file.txt@ 1 1 3 2 1363872334 1363872334 00000000000000000000000000000000 @//import/trunk/src/file.txt@ @1.1@ 1
// @pv@ 0 @db.revcx@ 2 @//import/branches/merge/src/file.txt@ 1 3

// Domain - A domain record (db.domain)
// Name	Type	Explanation
// name	Domain	Key: Name of domain.
// type	DomainType	Type of domain: client, label, branch, depot or typemap.
// host	Text	Associated host (optional).
// mount	Text	Client root.
// mount2	Text	Alternate client root.
// mount3	Text	Alternate client root.
// owner	User	Name of user who created the domain.
// updateDate	Date	Date of last update to domain specification.
// accessDate	Date	Date of last access to domain specification.
// options	DomainOpts	Options for client, label, and branch domains.
// mapState	MapState	The client map state.
// description	Text	Description of domain.

// Desc - A change description record (db.desc)
// Name	Type	Explanation
// descKey	Change	Key: Change number to which this description applies
// description	Text	Change description in full

// Change - A change record (db.change and db.changex)
// Name	Type	Explanation
// change	Change	Key: A change number
// descKey	Change	Change number of the first changelist with this description.
// (When a changelist is renumbered on submission, the description is preserved.)
// client	Domain	A client name; the client from which the change was submitted.
// user	User	A user name; the user who submitted the change.
// date	Date	The date at which the change was submitted.
// status	Status	Status of changelist: pending or committed.
// description	DescShort	Short description (first 31 characters) of change.
// The same data structure is used in both the db.change and db.changex files.

// Status - Status of a changelist
// Status	Explanation
// 0	pending (i.e. a pending changelist)
// 1	committed (i.e. a submitted changelist)

// Integed - A permanent integration record (db.integed)

// Name	Type	Explanation
// toFile	File	Key: File from which integration is being performed.
// fromFile	File	Secondary key: File to which integration is being performed.
// startFromRev	Rev	Tertiary key: Starting revision of fromFile
// endFromRev	Rev	Ending revision of fromFile.
// If integrating from a single revision (i.e. not a revision range),
// the startFromRev and endFromRev fields will be identical.
// startToRev	Rev	Start revision of toFile into which integration is being performed.
// endToRev	Rev	End revision of toFile into which integration is being performed. Only varies from startToRev for reverse integration records
// how	IntegHow	Integration method: variations on merge/branch/copy/ignore/delete.
// change	Change	Changelist associated with the integration.
// By way of example - here is the output of a p4 filelog on a file:

// ... #31 change 2552 integrate on 1997/01/12 by user@client (filetype) 'description of change'
// ... ... merge from //depot/main/p4-win/Jamfile#2,#4
// ... ... branch into //depot/r97.1/p4/Jamfile#1
// Field	Value
// FromFile	Jamfile
// ToFile	Jamfile
// startFromRev	2
// endFromRev	4
// toRev	31

// FileType - File type for client/server
// Storage Type	Comments
// FileStorageType:
// Value	Explanation
// 0x0000	RCS
// 0x0001	binary
// 0x0002	tiny (uncompressed)
// 0x0003	compressed
// 0x0005	(lbrtype only:) old onerev (deprecated)
// 0x0006	compressed temp object (deprecated)
// 0x000F	storage type bit mask
// FileType - file type for client/server:
// The layout is as follows:

//   0xXXXX
//     ||||
//     |||+- storage method
//     ||+-- storage method modifiers
//     |+--- client access method + modifiers
//     +---- client access modifiers known to server
// SetOld() handles conversion from when the type was a uchar and each indicator was only a nybble: 0xF0 was moved to 0xF00 and 0x02 was moved to 0x10.
// No one ever sets these values literally -- only symbolically. But we define them here for completeness.

// 0xFFFF stands for file type unset.

// FileStorageMods:
// Value	Explanation
// 0x0010	Keyed 99.2
// 0x0020	Keyed 2000.1
// 0x0030	Any keyed 2000.1
// 0x0040	Exclusive access
// 0x0080	Tempobj (+S) 2002.2
// 0x00F0	Storage modifier bit mask
// FileClientType:
// Value	Explanation
// 0x0000	Text
// 0x0100	Binary
// 0x0400	Symlink
// 0x0500	Resource
// 0x0800	Unicode
// 0x0900	RawText (no CRLF)
// 0x0C00	Mac data + rsrc text (2000.2)
// 0x0D00	Mac data + rsrc (99.2)
// 0x0200	Executable bit set
// 0x0D00	Client type bit mask
// 0x0F00	Client type (with mods) bit mask
// FileClientMods:
// Value	Explanation
// 0x1000	Always writable
// 0x2000	Save client mod time
// 0xF000	Client modifier bits

// TEXT("ltext", 0x00000001), // 'text+F' only ascii in sample
// BINARY("ubinary", 0x00000101), // 'binary+F'
// UTF8("unicode+F", 0x00080001), // 'unicode+F'
// UTF16("utf16+F", 0x01080001), // 'utf16+F'
// SYMLINK("symlink+F", 0x00040001), // 'symlink+F'

type FileType int

const (
	UText   FileType = 0x00000001 // text+F
	CText   FileType = 0x00000003 // text+C
	UBinary FileType = 0x00000101 // binary+F
	Binary  FileType = 0x00000103 // binary
)

type FileAction int

const (
	Add FileAction = iota
	Edit
	Delete
	Branch
	Integrate
)

type Journal struct {
	filename string
	w        io.Writer
}

var p4client = "git-client"
var p4user = "git-user"
var statusSubmitted = "1"

func (j *Journal) CreateJournal() {

	f, err := os.Create(j.filename)
	if err != nil {
		panic(err)
	}
	j.w = f
}

func (j *Journal) SetWriter(w io.Writer) {
	j.w = w
}

func (j *Journal) WriteHeader() {

	hdr := `@pv@ 0 @db.depot@ @import@ 0 @subdir@ @import/...@ 
@pv@ 3 @db.domain@ @import@ 100 @@ @@ @@ @@ @git-user@ 0 0 0 1 @Created by git-user@ 
@pv@ 3 @db.user@ @git-user@ @git-user@@git-client@ @@ 0 0 @git-user@ @@ 0 @@ 0 
@pv@ 0 @db.view@ @git-client@ 0 0 @//git-client/...@ @//import/...@ 
@pv@ 3 @db.domain@ @git-client@ 99 @@ @/ws@ @@ @@ @git-user@ 0 0 0 1 @Created by git-user@ 
`
	_, err := fmt.Fprint(j.w, hdr)
	if err != nil {
		panic(err)
	}

}

func (j *Journal) WriteChange(chgNo int, description string, chgTime int) {

	_, err := fmt.Fprintf(j.w, "@pv@ 0 @db.desc@ %d @%s@ \n", chgNo, description)
	if err != nil {
		panic(err)
	}

	// @pv@ 0 @db.change@ 1 1 @svn-client@ @pallen@ 1363872228 1 @add@
	_, err = fmt.Fprintf(j.w, "@pv@ 0 @db.change@ %d %d @%s@ @%s@ %d %s @%s@ \n",
		chgNo, chgNo, p4client, p4user, chgTime, statusSubmitted, description)
	if err != nil {
		panic(err)
	}

}

// Rev - A revision record (db.rev, db.revpx)
// Name			Type		Explanation
// ------------------------------------
// depotFile	File		Key: File name as it appears in the depot.
// depotRev		Rev			Secondary key: Revision number.
// type			FileType	Flags denoting file type.
// action		Action		Action performed on file: add, edit, delete, branch, integ, or import.
// change		Change		Changelist associated with this revision.
// date			Date		Date of changelist submission for this revision.
// modTime		Date		Date of last modification of the file when submitted.
// digest		Digest		MD5 digest of the full file at this revision level.
// 		This is usually generated by p4 verify, but if
// 		not generated, it will be created on-the-fly with commands
// 		like p4 diff, etc.
// traitlot		Int			Group of traits associated with file revision.
// lbrFile		File		Filename for librarian's purposes.
// 		Specifies location in the depot's data which, when used with the client root, holds the file.
// LbrRev		LbrRev		Revision number in the librarian's archive.
// lbrType		FileType	File type for librarian's purposes.

// RevCx - Revision change index (db.revcx)
// This is a subset of the information in the Rev table, but with different indexing.
// Name			Type		Explanation
// ------------------------------------
// change		Change		Key: Changelist associated with this revision
// depotFile	File		Secondary key: Filename in depot.
// depotRev		Rev			Revision number of the filename in depot.
// action		Action	File was opened for add/edit/delete/branch/integrate/import.

func (j *Journal) WriteRev(depotFile string, depotRev int, action FileAction, fileType FileType, chgNo int, chgTime int) {

	const md5 = "00000000000000000000000000000000"
	lbrType := fileType

	// @pv@ 3 @db.rev@ @//import/trunk/src/file.txt@ 1 1 0 1 1363872228 1363872228 00000000000000000000000000000000 @//import/trunk/src/file.txt@ @1.1@ 1
	_, err := fmt.Fprintf(j.w,
		"@pv@ 3 @db.rev@ @%s@ %d %d %d %d %d %d %s @%s@ @1.%d@ %d \n",
		depotFile, depotRev, fileType, action, chgNo, chgTime, chgTime, md5, depotFile, chgNo, lbrType)
	if err != nil {
		panic(err)
	}

	// @pv@ 0 @db.revcx@ 1 @//import/trunk/src/file.txt@ 1 0
	_, err = fmt.Fprintf(j.w,
		"@pv@ 0 @db.revcx@ %d @%s@ @1.%d@ %d \n",
		chgNo, depotFile, depotRev, action)
	if err != nil {
		panic(err)
	}

}
