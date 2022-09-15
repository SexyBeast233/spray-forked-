package pkg

import (
	"fmt"
	"github.com/chainreactors/gogo/v2/pkg/fingers"
	"github.com/go-dedup/simhash"
	"math/rand"
	"os"
	"strings"
	"time"
	"unsafe"
)

func HasStdin() bool {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}

	isPipedFromChrDev := (stat.Mode() & os.ModeCharDevice) == 0
	isPipedFromFIFO := (stat.Mode() & os.ModeNamedPipe) != 0

	return isPipedFromChrDev || isPipedFromFIFO
}

func Simhash(raw []byte) string {

	sh := simhash.NewSimhash()
	return fmt.Sprintf("%x", sh.GetSimhash(sh.NewWordFeatureSet(raw)))
}

const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

var src = rand.NewSource(time.Now().UnixNano())

const (
	// 6 bits to represent a letter index
	letterIdBits = 6
	// All 1-bits as many as letterIdBits
	letterIdMask = 1<<letterIdBits - 1
	letterIdMax  = 63 / letterIdBits
)

func RandPath() string {
	n := 16
	b := make([]byte, n)
	b[0] = byte(0x2f)
	// A rand.Int63() generates 63 random bits, enough for letterIdMax letters!
	for i, cache, remain := n-1, src.Int63(), letterIdMax; i >= 1; {
		if remain == 0 {
			cache, remain = src.Int63(), letterIdMax
		}
		if idx := int(cache & letterIdMask); idx < len(letters) {
			b[i] = letters[idx]
			i--
		}
		cache >>= letterIdBits
		remain--
	}
	return *(*string)(unsafe.Pointer(&b))
}

var Fingers fingers.Fingers

func FingerDetect(body []byte) Frameworks {
	var frames Frameworks
	content := string(body)
	for _, finger := range Fingers {
		frame, _, ok := fingers.FingerMatcher(finger, content, 0, nil)
		if ok {
			frames = append(frames, frame)
		}
	}
	return frames
}

type Frameworks []*fingers.Framework

func (fs Frameworks) ToString() string {
	frameworkStrs := make([]string, len(fs))
	for i, f := range fs {
		frameworkStrs[i] = "[" + f.ToString() + "]"
	}
	return strings.Join(frameworkStrs, " ")
}
