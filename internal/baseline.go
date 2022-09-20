package internal

import (
	"encoding/json"
	"fmt"
	"github.com/chainreactors/logs"
	"github.com/chainreactors/parsers"
	"github.com/chainreactors/spray/pkg"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func NewBaseline(u *url.URL, resp *http.Response) *baseline {
	bl := &baseline{
		Url:        u,
		UrlString:  u.String(),
		BodyLength: resp.ContentLength,
		Status:     resp.StatusCode,
		IsValid:    true,
	}

	var header strings.Builder
	for k, v := range resp.Header {
		for _, i := range v {
			header.WriteString(k)
			header.WriteString(": ")
			header.WriteString(i)
			header.WriteString("\r\n")
		}
	}
	bl.Header = header.String()
	bl.HeaderLength = header.Len()

	redirectURL, err := resp.Location()
	if err == nil {
		bl.RedirectURL = redirectURL.String()
	}

	body := make([]byte, 20480)
	if bl.BodyLength > 0 {
		n, err := io.ReadFull(resp.Body, body)
		if err == nil {
			bl.Body = body
		} else if err == io.ErrUnexpectedEOF {
			bl.Body = body[:n]
		} else {
			logs.Log.Error("readfull failed" + err.Error())
		}
	}
	if len(bl.Body) > 0 {
		bl.Md5 = parsers.Md5Hash(bl.Body)
		bl.Mmh3 = parsers.Mmh3Hash32(bl.Body)
		bl.Simhash = pkg.Simhash(bl.Body)
		if strings.Contains(string(bl.Body), bl.UrlString[1:]) {
			bl.IsDynamicUrl = true
		}
		// todo callback
	}

	// todo extract
	bl.Extracteds = pkg.Extractors.Extract(bl.Response)
	// todo 指纹识别
	bl.Frameworks = pkg.FingerDetect(bl.Response)
	return bl
}

func NewInvalidBaseline(u *url.URL, resp *http.Response) *baseline {
	bl := &baseline{
		Url:        u,
		UrlString:  u.String(),
		BodyLength: resp.ContentLength,
		Status:     resp.StatusCode,
		IsValid:    false,
	}

	redirectURL, err := resp.Location()
	if err == nil {
		bl.RedirectURL = redirectURL.String()
	}

	return bl
}

type baseline struct {
	Url          *url.URL       `json:"-"`
	UrlString    string         `json:"url_string"`
	Body         []byte         `json:"-"`
	BodyLength   int64          `json:"body_length"`
	Header       string         `json:"-"`
	Response     string         `json:"-"`
	HeaderLength int            `json:"header_length"`
	RedirectURL  string         `json:"redirect_url"`
	Status       int            `json:"status"`
	Md5          string         `json:"md5"`
	Mmh3         string         `json:"mmh3"`
	Simhash      string         `json:"simhash"`
	IsDynamicUrl bool           `json:"is_dynamic_url"` // 判断是否存在动态的url
	Spended      int            `json:"spended"`        // 耗时, 毫秒
	Frameworks   pkg.Frameworks `json:"frameworks"`
	Extracteds   pkg.Extracteds `json:"extracts"`
	Err          error          `json:"-"`
	IsValid      bool           `json:"-"`
}

func (bl *baseline) Compare(other *baseline) bool {
	if bl.Md5 == other.Md5 {
		return true
	}

	if bl.RedirectURL == other.RedirectURL {
		return true
	}

	return false
}

func (bl *baseline) FuzzyCompare() bool {
	// todo 模糊匹配
	return false
}

func (bl *baseline) String() string {
	var line strings.Builder
	line.WriteString("[+] ")
	line.WriteString(bl.UrlString)
	line.WriteString(fmt.Sprintf(" - %d - %d ", bl.Status, bl.BodyLength))
	if bl.RedirectURL != "" {
		line.WriteString("-> ")
		line.WriteString(bl.RedirectURL)
	}
	line.WriteString(bl.Frameworks.ToString())
	//line.WriteString(bl.Extracteds)
	return line.String()
}

func (bl *baseline) Jsonify() string {
	bs, err := json.Marshal(bl)
	if err != nil {
		return ""
	}
	return string(bs)
}
