package main

/*
#include <stdlib.h>
*/
import "C"

import "unsafe"

func unsafePointer(ptr *C.char) unsafe.Pointer {
	return unsafe.Pointer(ptr)
}

func unsafeByteSlice(ptr *C.char, length C.int) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(ptr)), int(length))
}
