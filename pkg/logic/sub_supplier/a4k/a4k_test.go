package a4k

import (
	"path/filepath"
	"testing"

	"github.com/allanpk716/ChineseSubFinder/pkg/logic/file_downloader"
	"github.com/allanpk716/ChineseSubFinder/pkg/logic/series_helper"
	"github.com/allanpk716/ChineseSubFinder/pkg/sub_helper"
	"github.com/allanpk716/ChineseSubFinder/pkg/unit_test_helper"

	"github.com/allanpk716/ChineseSubFinder/pkg/cache_center"
	"github.com/allanpk716/ChineseSubFinder/pkg/global_value"
	"github.com/allanpk716/ChineseSubFinder/pkg/log_helper"
	"github.com/allanpk716/ChineseSubFinder/pkg/my_util"
	"github.com/allanpk716/ChineseSubFinder/pkg/random_auth_key"

	"github.com/allanpk716/ChineseSubFinder/pkg/settings"
)

func TestSupplier_searchKeyword(t *testing.T) {

	keyword := "Spider-Man: No Way Home 2021"
	defInstance()
	gotOutSubInfos, err := a4kInstance.searchKeyword(keyword, false)
	if err != nil {
		t.Fatal(err)
	}

	for i, searchResultItem := range gotOutSubInfos {
		println(i, searchResultItem.Title)
	}
}

func TestSupplier_GetSubListFromFile4Movie(t *testing.T) {

	videoFPath := "X:\\电影\\失控玩家 (2021)\\失控玩家 (2021).mp4"
	defInstance()

	gots, err := a4kInstance.GetSubListFromFile4Movie(videoFPath)
	if err != nil {
		t.Fatal(err)
	}
	for i, got := range gots {
		println(i, got.Name, len(got.Data), got.Ext)
	}
}

func TestSupplier_GetSubListFromFile4Series(t *testing.T) {

	epsMap := make(map[int][]int, 0)
	epsMap[4] = []int{1}
	//epsMap[1] = []int{1, 2, 3}
	rootDir := unit_test_helper.GetTestDataResourceRootPath([]string{"sub_spplier"}, 5, true)
	ser := filepath.Join(rootDir, "zimuku", "series", "黄石 (2018)")
	// 读取本地的视频和字幕信息
	seriesInfo, err := series_helper.ReadSeriesInfoFromDir(log_helper.GetLogger4Tester(), ser,
		90, false, false, settings.GetSettings().AdvancedSettings.ProxySettings, epsMap)
	if err != nil {
		t.Fatal(err)
	}

	defInstance()

	gots, err := a4kInstance.GetSubListFromFile4Series(seriesInfo)
	if err != nil {
		t.Fatal(err)
	}
	for i, got := range gots {
		println(i, got.Name, len(got.Data), got.Ext)
	}

	organizeSubFiles, err := sub_helper.OrganizeDlSubFiles(log_helper.GetLogger4Tester(), filepath.Base(seriesInfo.DirPath), gots, false)
	if err != nil {
		t.Fatal(err)
	}
	for i, got := range organizeSubFiles {
		for j, s := range got {
			println(i, j, s)
		}
	}
}

var a4kInstance *Supplier

func defInstance() {

	my_util.ReadCustomAuthFile(log_helper.GetLogger4Tester())

	authKey := random_auth_key.AuthKey{
		BaseKey:  global_value.BaseKey(),
		AESKey16: global_value.AESKey16(),
		AESIv16:  global_value.AESIv16(),
	}

	nowSettings := settings.GetSettings()
	nowSettings.ExperimentalFunction.ShareSubSettings.ShareSubEnabled = true

	a4kInstance = NewSupplier(file_downloader.NewFileDownloader(
		cache_center.NewCacheCenter("test", nowSettings, log_helper.GetLogger4Tester()), authKey))
}