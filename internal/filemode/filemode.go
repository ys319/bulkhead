// Package filemode says whether a file's permission bits mean anything on
// the system bh runs on.
package filemode

import "runtime"

// Kept: the file system keeps permission bits, and they say who may read a
// file.
//
// Not on Windows. Go reports 0666 there for every file (0444 for a
// read-only one) and Chmod changes nothing else; who may read a file is
// decided by its access control list, which bh neither sets nor inspects.
// Files under the user's profile are the user's by inheritance, and the ssh
// client looks at the list of a private key itself before it uses it.
//
// It is what the program runs on that counts, not what a test has told it
// to behave like: the files are real either way.
const Kept = runtime.GOOS != "windows"
