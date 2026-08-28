# Filetype info in checkpoint/journal entries

## FileType - File type for client/server

```C++
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
```

Bits used to store the server's storage type (Mask: 0x000F)

```
Value	Explanation
0x0000	RCS
0x0001	Binary
0x0002	Tiny
0x0003	Compressed
0x0004	tempobj (deprecated since 2002.1)
0x0005	Detect type. Used internally when no type is given by the user.
0x0006	Compressed tempobj (deprecated since 2002.1)
0x0007	Binary access to compressed data. Used internally.
0x0008	Stored externally via librarian trigger
```

Bits used to store the modifiers to the server's storage type (Mask: 0x00F0)

```
Value	Explanation
0x0010	99.2-style keyword expansion/contraction
0x0020	2000.1-style keyword expansion/contraction
0x0030	Any type of keyword expansion
0x0040	Exclusive open
0x0080	New tempobj (+S modifier)
```

Bits used to store the file type from the client's point of view. (Mask: 0x10D0000)

```
Value	Explanation
0x0000000	text
0x0010000	binary
0x0020000	executable bit set
0x0040000	symlink
0x0050000	resource fork
0x0080000	unicode
0x0090000	raw text (nocrlf - 99.1)
0x00C0000	apple data + resource file (since 2000.2)
0x00D0000	apple data + resource file (99.2)
0x1000000	Used to support filetype detection
0x1010000	AutoWild
0x1040000	utf8
0x1050000	submodule
0x1080000	utf16 client utf8 server (2007.2)
```

## Examples

* Rev 2 is text+c
* Rev 1 is text (RCS)

```
@pv@ 9 @db.rev@ @//stream/main/test.txt@ 2 0 1 2 1649076856 1649076849 683F7548052375929176FED651B389D4 10 0 0 @//stream/main/test.txt@ @1.2@ 3
@pv@ 9 @db.rev@ @//stream/main/test.txt@ 1 0 0 1 1649076822 1649076817 D8E8FCA2DC0F896FD7CB4CB0031BA249 5 0 0 @//stream/main/test.txt@ @1.1@ 0

Filetype    LbrType     Displayed
0           3           headType text   lbrType text+C
0           0           headType text   lbrType text

/tmp/dvcs2$ p4 fstat -Ob @=6 |egrep "depotFile|Type"
... depotFile //stream/main/bin_comp.dat
... headType binary
... lbrType binary
... depotFile //stream/main/bin_uncomp.dat
... headType binary+F
... lbrType binary+F
/tmp/dvcs2$ find .p4root/stream -type f
.p4root/stream/main/test.txt,d/1.2.gz
.p4root/stream/main/test.txt,v
.p4root/stream/main/bin_uncomp.dat,d/1.6
.p4root/stream/main/bin_comp.dat,d/1.6.gz

@pv@ 9 @db.rev@ @//stream/main/bin_comp.dat@ 1 65539 0 6 1649428734 1649428636 0FBE280319E719B7E93857949BBBC798 9 0 0 @//stream/main/bin_comp.dat@ @1.6@ 65539
@pv@ 9 @db.rev@ @//stream/main/bin_uncomp.dat@ 1 65537 0 6 1649428734 1649428681 0FBE280319E719B7E93857949BBBC798 9 0 0 @//stream/main/bin_uncomp.dat@ @1.6@ 65537

Filetype    LbrType     Displayed
65539       65539       headType binary     lbrType binary
65537       65537       headType binary+F   lbrType binary+F

65539 = 0x10003
65537 = 0x10001
```
