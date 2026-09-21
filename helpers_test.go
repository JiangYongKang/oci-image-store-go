package ocistore

import "strconv"

func itoa(i int) string { return strconv.Itoa(i) }

func repeat64(c string) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c[0]
	}
	return string(b)
}
