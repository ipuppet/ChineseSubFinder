package rod_helper

import (
	"testing"
	"time"

	"github.com/allanpk716/ChineseSubFinder/pkg/log_helper"
	"github.com/allanpk716/ChineseSubFinder/pkg/settings"
)

func TestNewMultiBrowser(t *testing.T) {

	browserOptions := NewBrowserOptions(log_helper.GetLogger4Tester(), true, settings.GetSettings())
	browserOptions.SetXrayPoolUrl("127.0.0.1")
	browserOptions.SetXrayPoolPort("19035")
	b := NewMultiBrowser(browserOptions)

	for i := 0; i < 5; i++ {
		page, err := NewPageNavigateWithProxy(b.GetOneBrowser(), b.LbHttpUrl, "https://www.ipaddress.my/", 10*time.Second)
		if err != nil {
			return
		}
		page.Close()
	}

	println(b)
}
