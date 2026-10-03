package duration_test

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/nrynss/keel/duration"
)

func ExampleRead() {
	b := make([]byte, 44+16000)
	copy(b[0:4], "RIFF")
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
	copy(b[8:12], "WAVE")
	copy(b[12:16], "fmt ")
	binary.LittleEndian.PutUint32(b[16:20], 16)
	binary.LittleEndian.PutUint16(b[20:22], 1)
	binary.LittleEndian.PutUint16(b[22:24], 1)
	binary.LittleEndian.PutUint32(b[24:28], 8000)
	binary.LittleEndian.PutUint32(b[28:32], 16000)
	binary.LittleEndian.PutUint16(b[32:34], 2)
	binary.LittleEndian.PutUint16(b[34:36], 16)
	copy(b[36:40], "data")
	binary.LittleEndian.PutUint32(b[40:44], 16000)

	d, err := duration.Read(b)
	if err != nil {
		fmt.Println("read failed:", err)
		return
	}
	fmt.Println(d == time.Second)
	// Output:
	// true
}
