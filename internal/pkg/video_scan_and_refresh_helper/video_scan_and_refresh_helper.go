package video_scan_and_refresh_helper

import (
	"github.com/allanpk716/ChineseSubFinder/internal/dao"
	embyHelper "github.com/allanpk716/ChineseSubFinder/internal/logic/emby_helper"
	"github.com/allanpk716/ChineseSubFinder/internal/logic/file_downloader"
	"github.com/allanpk716/ChineseSubFinder/internal/logic/forced_scan_and_down_sub"
	"github.com/allanpk716/ChineseSubFinder/internal/logic/restore_fix_timeline_bk"
	seriesHelper "github.com/allanpk716/ChineseSubFinder/internal/logic/series_helper"
	subSupplier "github.com/allanpk716/ChineseSubFinder/internal/logic/sub_supplier"
	"github.com/allanpk716/ChineseSubFinder/internal/logic/sub_supplier/xunlei"
	"github.com/allanpk716/ChineseSubFinder/internal/pkg/decode"
	"github.com/allanpk716/ChineseSubFinder/internal/pkg/imdb_helper"
	"github.com/allanpk716/ChineseSubFinder/internal/pkg/my_util"
	"github.com/allanpk716/ChineseSubFinder/internal/pkg/settings"
	subTimelineFixerPKG "github.com/allanpk716/ChineseSubFinder/internal/pkg/sub_timeline_fixer"
	"github.com/allanpk716/ChineseSubFinder/internal/pkg/task_control"
	"github.com/allanpk716/ChineseSubFinder/internal/pkg/task_queue"
	"github.com/allanpk716/ChineseSubFinder/internal/types/backend"
	"github.com/allanpk716/ChineseSubFinder/internal/types/common"
	"github.com/allanpk716/ChineseSubFinder/internal/types/emby"
	TTaskqueue "github.com/allanpk716/ChineseSubFinder/internal/types/task_queue"
	"github.com/emirpasic/gods/maps/treemap"
	"github.com/huandu/go-clone"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"path/filepath"
	"strings"
	"sync"
)

type VideoScanAndRefreshHelper struct {
	settings                 *settings.Settings              // 设置的实例
	log                      *logrus.Logger                  // 日志实例
	fileDownloader           *file_downloader.FileDownloader // 文件下载器
	NeedForcedScanAndDownSub bool                            // 将会强制扫描所有的视频，下载字幕，替换已经存在的字幕，不进行时间段和已存在则跳过的判断。且不会进过 Emby API 的逻辑，智能进行强制去以本程序的方式去扫描。
	NeedRestoreFixTimeLineBK bool                            // 从 csf-bk 文件还原时间轴修复前的字幕文件
	embyHelper               *embyHelper.EmbyHelper          // Emby 的实例
	downloadQueue            *task_queue.TaskQueue           // 需要下载的视频的队列
	subSupplierHub           *subSupplier.SubSupplierHub     // 字幕提供源的集合，仅仅是 check 是否需要下载字幕是足够的，如果要下载则需要额外的初始化和检查
	taskControl              *task_control.TaskControl       // 任务控制器
}

func NewVideoScanAndRefreshHelper(fileDownloader *file_downloader.FileDownloader, downloadQueue *task_queue.TaskQueue) *VideoScanAndRefreshHelper {
	v := VideoScanAndRefreshHelper{settings: fileDownloader.Settings, log: fileDownloader.Log, downloadQueue: downloadQueue,
		subSupplierHub: subSupplier.NewSubSupplierHub(
			xunlei.NewSupplier(fileDownloader),
		)}

	var err error
	v.taskControl, err = task_control.NewTaskControl(4, v.log)
	if err != nil {
		fileDownloader.Log.Panicln(err)
	}
	return &v
}

func (v *VideoScanAndRefreshHelper) Start() error {

	defer func() {
		v.log.Infoln("Video Scan End")
		v.log.Infoln("------------------------------------")
	}()

	v.log.Infoln("------------------------------------")
	v.log.Infoln("Video Scan Started...")
	// 先进行扫描
	scanResult, err := v.ScanNormalMovieAndSeries()
	if err != nil {
		v.log.Errorln("ScanNormalMovieAndSeries", err)
		return err
	}
	err = v.ScanEmbyMovieAndSeries(scanResult)
	if err != nil {
		v.log.Errorln("ScanEmbyMovieAndSeries", err)
		return err
	}
	// 过滤出需要下载的视频有那些，并放入队列中
	err = v.FilterMovieAndSeriesNeedDownload(scanResult)
	if err != nil {
		v.log.Errorln("FilterMovieAndSeriesNeedDownload", err)
		return err
	}

	return nil
}

func (v VideoScanAndRefreshHelper) Cancel() {
	v.taskControl.Release()
	v.taskControl.Reboot()
}

// ReadSpeFile 优先级最高。读取特殊文件，启用一些特殊的功能，比如 forced_scan_and_down_sub
func (v *VideoScanAndRefreshHelper) ReadSpeFile() error {
	// 理论上是一次性的，用了这个文件就应该没了
	// 强制的字幕扫描
	needProcessForcedScanAndDownSub, err := forced_scan_and_down_sub.CheckSpeFile()
	if err != nil {
		return err
	}
	v.NeedForcedScanAndDownSub = needProcessForcedScanAndDownSub
	// 从 csf-bk 文件还原时间轴修复前的字幕文件
	needProcessRestoreFixTimelineBK, err := restore_fix_timeline_bk.CheckSpeFile()
	if err != nil {
		return err
	}
	v.NeedRestoreFixTimeLineBK = needProcessRestoreFixTimelineBK

	v.log.Infoln("NeedRestoreFixTimeLineBK ==", needProcessRestoreFixTimelineBK)

	return nil
}

// ScanNormalMovieAndSeries 没有媒体服务器，扫描出有那些电影、连续剧需要进行字幕下载的
func (v *VideoScanAndRefreshHelper) ScanNormalMovieAndSeries() (*ScanVideoResult, error) {

	var err error
	outScanVideoResult := ScanVideoResult{}
	// ------------------------------------------------------------------------------
	// 由于需要进行视频信息的缓存，用于后续的逻辑，那么本地视频的扫描默认都会进行
	normalScanResult := NormalScanVideoResult{}
	// 直接由本程序自己去扫描视频视频有哪些
	// 全扫描
	if v.NeedForcedScanAndDownSub == true {
		v.log.Infoln("Forced Scan And DownSub")
	}
	wg := sync.WaitGroup{}
	var errMovie, errSeries error
	wg.Add(1)
	go func() {
		// --------------------------------------------------
		// 电影
		// 没有填写 emby_helper api 的信息，那么就走常规的全文件扫描流程
		normalScanResult.MoviesDirMap, errMovie = my_util.SearchMatchedVideoFileFromDirs(v.log, v.settings.CommonSettings.MoviePaths)
		wg.Done()
	}()
	wg.Add(1)
	go func() {
		// --------------------------------------------------
		// 连续剧
		// 遍历连续剧总目录下的第一层目录
		normalScanResult.SeriesDirMap, errSeries = seriesHelper.GetSeriesListFromDirs(v.log, v.settings.CommonSettings.SeriesPaths)
		// ------------------------------------------------------------------------------
		// 输出调试信息，有那些连续剧文件夹名称
		normalScanResult.SeriesDirMap.Each(func(key interface{}, value interface{}) {
			for i, s := range value.([]string) {
				v.log.Debugln("embyHelper == nil GetSeriesList", i, s)
			}
		})
		wg.Done()
	}()
	wg.Wait()
	if errMovie != nil {
		return nil, errMovie
	}
	if errSeries != nil {
		return nil, errSeries
	}
	// ------------------------------------------------------------------------------
	outScanVideoResult.Normal = &normalScanResult
	// ------------------------------------------------------------------------------
	// 将扫描到的信息缓存到本地中，用于后续的 Video 展示界面 和 Emby IMDB ID 匹配进行路径的转换
	err = v.updateLocalVideoCacheInfo(&outScanVideoResult)
	if err != nil {
		return nil, err
	}

	return &outScanVideoResult, nil
}

// ScanEmbyMovieAndSeries Emby媒体服务器，扫描出有那些电影、连续剧需要进行字幕下载的
func (v *VideoScanAndRefreshHelper) ScanEmbyMovieAndSeries(scanVideoResult *ScanVideoResult) error {

	if v.settings.EmbySettings.Enable == false {
		v.embyHelper = nil
	} else {

		if v.NeedForcedScanAndDownSub == true {
			// 如果是强制，那么就临时修改 Setting 的 Emby MaxRequestVideoNumber 参数为 1000000
			tmpSetting := clone.Clone(v.settings).(*settings.Settings)
			tmpSetting.EmbySettings.MaxRequestVideoNumber = 1000000
			v.embyHelper = embyHelper.NewEmbyHelper(v.log, tmpSetting)
		} else {
			v.embyHelper = embyHelper.NewEmbyHelper(v.log, v.settings)
		}
	}
	var err error

	// ------------------------------------------------------------------------------
	// 从 Emby 获取视频
	if v.embyHelper != nil {
		// TODO 如果后续支持了 Jellyfin、Plex 那么这里需要额外正在对应的扫描逻辑
		// 进过 emby_helper api 的信息读取
		embyScanResult := EmbyScanVideoResult{}
		v.log.Infoln("Movie Sub Dl From Emby API...")
		// Emby 情况，从 Emby 获取视频信息
		err = v.refreshEmbySubList()
		if err != nil {
			v.log.Errorln("refreshEmbySubList", err)
			return err
		}
		// ------------------------------------------------------------------------------
		// 有哪些更新的视频列表，包含电影、连续剧
		embyScanResult.MovieSubNeedDlEmbyMixInfoList, embyScanResult.SeriesSubNeedDlEmbyMixInfoMap, err = v.getUpdateVideoListFromEmby()
		if err != nil {
			v.log.Errorln("getUpdateVideoListFromEmby", err)
			return err
		}
		// ------------------------------------------------------------------------------
		scanVideoResult.Emby = &embyScanResult
	}

	return nil
}

// FilterMovieAndSeriesNeedDownload 过滤出需要下载字幕的视频，比如是否跳过中文的剧集，是否超过3个月的下载时间，丢入队列中
func (v *VideoScanAndRefreshHelper) FilterMovieAndSeriesNeedDownload(scanVideoResult *ScanVideoResult) error {

	if scanVideoResult.Normal != nil && v.settings.EmbySettings.Enable == false {
		err := v.filterMovieAndSeriesNeedDownloadNormal(scanVideoResult.Normal)
		if err != nil {
			return err
		}
	}

	if scanVideoResult.Emby != nil && v.settings.EmbySettings.Enable == true {
		err := v.filterMovieAndSeriesNeedDownloadEmby(scanVideoResult.Emby)
		if err != nil {
			return err
		}
	}

	return nil
}

func (v *VideoScanAndRefreshHelper) ScrabbleUpVideoList(scanVideoResult *ScanVideoResult, pathUrlMap map[string]string) ([]backend.MovieInfo, []backend.SeasonInfo) {

	if scanVideoResult.Normal != nil && v.settings.EmbySettings.Enable == false {
		return v.scrabbleUpVideoListNormal(scanVideoResult.Normal, pathUrlMap)
	}

	if scanVideoResult.Emby != nil && v.settings.EmbySettings.Enable == true {
		return v.scrabbleUpVideoListEmby(scanVideoResult.Emby, pathUrlMap)
	}

	return nil, nil
}

func (v *VideoScanAndRefreshHelper) scrabbleUpVideoListNormal(normal *NormalScanVideoResult, pathUrlMap map[string]string) ([]backend.MovieInfo, []backend.SeasonInfo) {

	movieInfos := make([]backend.MovieInfo, 0)
	seasonInfos := make([]backend.SeasonInfo, 0)

	if normal == nil {
		return movieInfos, seasonInfos
	}
	// 电影
	normal.MoviesDirMap.Each(func(movieDirRootPath interface{}, movieFPath interface{}) {

		oneMovieDirRootPath := movieDirRootPath.(string)
		for _, oneMovieFPath := range movieFPath.([]string) {

			desUrl, found := pathUrlMap[oneMovieDirRootPath]
			if found == false {
				// 没有找到对应的 URL
				continue
			}
			// 匹配上了前缀就替换这个，并记录
			movieFUrl := strings.ReplaceAll(oneMovieFPath, oneMovieDirRootPath, desUrl)
			oneMovieInfo := backend.MovieInfo{
				Name:       filepath.Base(movieFUrl),
				DirRootUrl: filepath.Dir(movieFUrl),
				VideoFPath: oneMovieFPath,
				VideoUrl:   movieFUrl,
			}
			movieInfos = append(movieInfos, oneMovieInfo)
		}
	})
	// 连续剧
	// seriesDirMap: dir <--> seriesList
	normal.SeriesDirMap.Each(func(seriesRootPathName interface{}, seriesNames interface{}) {

		oneSeriesRootPathName := seriesRootPathName.(string)
		for _, oneSeriesRootDir := range seriesNames.([]string) {

			desUrl, found := pathUrlMap[oneSeriesRootPathName]
			if found == false {
				// 没有找到对应的 URL
				continue
			}
			bNeedDlSub, seriesInfo, err := v.subSupplierHub.SeriesNeedDlSub(oneSeriesRootDir,
				v.NeedForcedScanAndDownSub, false)
			if err != nil {
				v.log.Errorln("filterMovieAndSeriesNeedDownloadNormal.SeriesNeedDlSub", err)
				continue
			}
			if bNeedDlSub == false {
				continue
			}
			seriesDirRootFUrl := strings.ReplaceAll(oneSeriesRootDir, oneSeriesRootPathName, desUrl)
			oneSeasonInfo := backend.SeasonInfo{
				Name:          filepath.Base(oneSeriesRootDir),
				RootDirPath:   oneSeriesRootDir,
				DirRootUrl:    seriesDirRootFUrl,
				OneVideoInfos: make([]backend.OneVideoInfo, 0),
			}
			for _, epsInfo := range seriesInfo.EpList {

				videoFUrl := strings.ReplaceAll(epsInfo.FileFullPath, oneSeriesRootPathName, desUrl)
				oneVideoInfo := backend.OneVideoInfo{
					Name:       epsInfo.Title,
					VideoFPath: epsInfo.FileFullPath,
					VideoUrl:   videoFUrl,
					Season:     epsInfo.Season,
					Episode:    epsInfo.Episode,
				}
				oneSeasonInfo.OneVideoInfos = append(oneSeasonInfo.OneVideoInfos, oneVideoInfo)
			}

			seasonInfos = append(seasonInfos, oneSeasonInfo)
		}
	})

	return movieInfos, seasonInfos
}

func (v VideoScanAndRefreshHelper) scrabbleUpVideoListEmby(emby *EmbyScanVideoResult, pathUrlMap map[string]string) ([]backend.MovieInfo, []backend.SeasonInfo) {

	movieInfos := make([]backend.MovieInfo, 0)
	seasonInfos := make([]backend.SeasonInfo, 0)

	if emby == nil {
		return movieInfos, seasonInfos
	}

	return movieInfos, seasonInfos
}

func (v *VideoScanAndRefreshHelper) refreshEmbySubList() error {

	if v.embyHelper == nil {
		return nil
	}

	bRefresh := false
	defer func() {
		if bRefresh == true {
			v.log.Infoln("Refresh Emby Sub List Success")
		} else {
			v.log.Errorln("Refresh Emby Sub List Error")
		}
	}()
	v.log.Infoln("Refresh Emby Sub List Start...")
	//------------------------------------------------------
	bRefresh, err := v.embyHelper.RefreshEmbySubList()
	if err != nil {
		return err
	}

	return nil
}

// updateLocalVideoCacheInfo 将扫描到的信息缓存到本地中，用于后续的 Video 展示界面 和 Emby IMDB ID 匹配进行路径的转换
func (v *VideoScanAndRefreshHelper) updateLocalVideoCacheInfo(scanVideoResult *ScanVideoResult) error {
	// 这里只使用 Normal 情况下获取到的信息
	if scanVideoResult.Normal == nil {
		return nil
	}
	// ------------------------------------------------------------------------------
	// 电影
	movieProcess := func(ctx context.Context, inData interface{}) error {

		taskData := inData.(*task_control.TaskData)
		movieInputData := taskData.DataEx.(TaskInputData)
		v.log.Infoln("updateLocalVideoCacheInfo", movieInputData.Index, movieInputData.InputPath)
		videoImdbInfo, err := decode.GetImdbInfo4Movie(movieInputData.InputPath)
		if err != nil {
			// 允许的错误，跳过，继续进行文件名的搜索
			v.log.Warningln("GetImdbInfo4Movie", movieInputData.Index, err)
			return err
		}
		// 获取 IMDB 信息
		localIMDBInfo, err := imdb_helper.GetVideoIMDBInfoFromLocal(v.log, videoImdbInfo)
		if err != nil {
			v.log.Warningln("GetVideoIMDBInfoFromLocal,IMDB:", videoImdbInfo.ImdbId, movieInputData.InputPath, err)
			return err
		}

		movieDirPath := filepath.Dir(movieInputData.InputPath)
		if (movieDirPath != "" && localIMDBInfo.RootDirPath != movieDirPath) || localIMDBInfo.IsMovie != true {
			// 更新数据
			localIMDBInfo.RootDirPath = movieDirPath
			localIMDBInfo.IsMovie = true
			dao.GetDb().Save(localIMDBInfo)
		}

		return nil
	}
	// ------------------------------------------------------------------------------
	v.taskControl.SetCtxProcessFunc("updateLocalVideoCacheInfo", movieProcess, common.ScanPlayedSubTimeOut)
	// ------------------------------------------------------------------------------
	scanVideoResult.Normal.MoviesDirMap.Any(func(movieDirRootPath interface{}, movieFPath interface{}) bool {

		//oneMovieDirRootPath := movieDirRootPath.(string)
		for i, oneMovieFPath := range movieFPath.([]string) {
			err := v.taskControl.Invoke(&task_control.TaskData{
				Index: i,
				Count: len(movieFPath.([]string)),
				DataEx: TaskInputData{
					Index:     i,
					InputPath: oneMovieFPath,
				},
			})
			if err != nil {
				v.log.Errorln("updateLocalVideoCacheInfo.MoviesDirMap.Invoke", err)
				return true
			}
		}

		return false
	})
	v.taskControl.Hold()
	// ------------------------------------------------------------------------------
	seriesProcess := func(ctx context.Context, inData interface{}) error {

		taskData := inData.(*task_control.TaskData)
		seriesInputData := taskData.DataEx.(TaskInputData)
		v.log.Infoln("updateLocalVideoCacheInfo", seriesInputData.Index, seriesInputData.InputPath)

		videoInfo, err := decode.GetImdbInfo4SeriesDir(seriesInputData.InputPath)
		if err != nil {
			v.log.Warningln("GetImdbInfo4SeriesDir", seriesInputData.InputPath, err)
			return err
		}

		// 获取 IMDB 信息
		localIMDBInfo, err := imdb_helper.GetVideoIMDBInfoFromLocal(v.log, videoInfo)
		if err != nil {
			v.log.Warningln("GetVideoIMDBInfoFromLocal,IMDB:", videoInfo.ImdbId, seriesInputData.InputPath, err)
			return err
		}
		if (seriesInputData.InputPath != "" && localIMDBInfo.RootDirPath != seriesInputData.InputPath) || localIMDBInfo.IsMovie != false {
			// 更新数据
			localIMDBInfo.RootDirPath = seriesInputData.InputPath
			localIMDBInfo.IsMovie = false
			dao.GetDb().Save(localIMDBInfo)
		}

		return nil
	}
	// ------------------------------------------------------------------------------
	v.taskControl.SetCtxProcessFunc("updateLocalVideoCacheInfo", seriesProcess, common.ScanPlayedSubTimeOut)
	// ------------------------------------------------------------------------------
	// 连续剧
	scanVideoResult.Normal.SeriesDirMap.Each(func(seriesRootPathName interface{}, seriesNames interface{}) {

		for i, oneSeriesRootDir := range seriesNames.([]string) {
			err := v.taskControl.Invoke(&task_control.TaskData{
				Index: i,
				Count: len(seriesNames.([]string)),
				DataEx: TaskInputData{
					Index:     i,
					InputPath: oneSeriesRootDir,
				},
			})
			if err != nil {
				v.log.Errorln(err)
				return
			}
		}
	})
	v.taskControl.Hold()

	return nil
}

func (v *VideoScanAndRefreshHelper) filterMovieAndSeriesNeedDownloadNormal(normal *NormalScanVideoResult) error {
	// ----------------------------------------
	// Normal 过滤，电影
	movieProcess := func(ctx context.Context, inData interface{}) error {

		taskData := inData.(*task_control.TaskData)
		movieInputData := taskData.DataEx.(TaskInputData)
		if v.subSupplierHub.MovieNeedDlSub(movieInputData.InputPath, v.NeedForcedScanAndDownSub) == false {
			return nil
		}
		bok, err := v.downloadQueue.Add(*TTaskqueue.NewOneJob(
			common.Movie, movieInputData.InputPath, task_queue.DefaultTaskPriorityLevel,
		))
		if err != nil {
			v.log.Errorln("filterMovieAndSeriesNeedDownloadNormal.Movie.NewOneJob", err)
			return err
		}
		if bok == false {
			v.log.Warningln(common.Movie.String(), movieInputData.InputPath, "downloadQueue isExisted")
		}

		return nil
	}
	// ----------------------------------------
	v.taskControl.SetCtxProcessFunc("updateLocalVideoCacheInfo", movieProcess, common.ScanPlayedSubTimeOut)
	// ----------------------------------------
	normal.MoviesDirMap.Any(func(movieDirRootPath interface{}, movieFPath interface{}) bool {

		//oneMovieDirRootPath := movieDirRootPath.(string)
		for i, oneMovieFPath := range movieFPath.([]string) {
			// 放入队列
			err := v.taskControl.Invoke(&task_control.TaskData{
				Index: i,
				Count: len(movieFPath.([]string)),
				DataEx: TaskInputData{
					Index:     i,
					InputPath: oneMovieFPath,
				},
			})
			if err != nil {
				v.log.Errorln(err)
				return true
			}
		}

		return false
	})
	v.taskControl.Hold()
	// ----------------------------------------
	// Normal 过滤，连续剧
	seriesProcess := func(ctx context.Context, inData interface{}) error {

		taskData := inData.(*task_control.TaskData)
		seriesInputData := taskData.DataEx.(TaskInputData)
		// 因为可能回去 Web 获取 IMDB 信息，所以这里的错误不返回
		bNeedDlSub, seriesInfo, err := v.subSupplierHub.SeriesNeedDlSub(seriesInputData.InputPath,
			v.NeedForcedScanAndDownSub, false)
		if err != nil {
			v.log.Errorln("filterMovieAndSeriesNeedDownloadNormal.SeriesNeedDlSub", err)
			return err
		}
		if bNeedDlSub == false {
			return nil
		}

		for _, episodeInfo := range seriesInfo.NeedDlEpsKeyList {
			// 放入队列
			oneJob := TTaskqueue.NewOneJob(
				common.Series, episodeInfo.FileFullPath, task_queue.DefaultTaskPriorityLevel,
			)
			oneJob.Season = episodeInfo.Season
			oneJob.Episode = episodeInfo.Episode
			oneJob.SeriesRootDirPath = seriesInfo.DirPath

			bok, err := v.downloadQueue.Add(*oneJob)
			if err != nil {
				v.log.Errorln("filterMovieAndSeriesNeedDownloadNormal.Series.NewOneJob", err)
				continue
			}
			if bok == false {
				v.log.Warningln(common.Series.String(), episodeInfo.FileFullPath, "downloadQueue isExisted")
			}
		}

		return nil
	}
	// ----------------------------------------
	v.taskControl.SetCtxProcessFunc("updateLocalVideoCacheInfo", seriesProcess, common.ScanPlayedSubTimeOut)
	// ----------------------------------------
	// seriesDirMap: dir <--> seriesList
	normal.SeriesDirMap.Each(func(seriesRootPathName interface{}, seriesNames interface{}) {

		for i, oneSeriesRootDir := range seriesNames.([]string) {

			// 放入队列
			err := v.taskControl.Invoke(&task_control.TaskData{
				Index: i,
				Count: len(seriesNames.([]string)),
				DataEx: TaskInputData{
					Index:     i,
					InputPath: oneSeriesRootDir,
				},
			})
			if err != nil {
				v.log.Errorln(err)
				return
			}
		}
	})
	v.taskControl.Hold()
	// ----------------------------------------
	return nil
}

func (v *VideoScanAndRefreshHelper) filterMovieAndSeriesNeedDownloadEmby(emby *EmbyScanVideoResult) error {
	// ----------------------------------------
	// Emby 过滤，电影
	for _, oneMovieMixInfo := range emby.MovieSubNeedDlEmbyMixInfoList {
		// 放入队列
		if v.subSupplierHub.MovieNeedDlSub(oneMovieMixInfo.PhysicalVideoFileFullPath, v.NeedForcedScanAndDownSub) == false {
			continue
		}
		bok, err := v.downloadQueue.Add(*TTaskqueue.NewOneJob(
			common.Movie, oneMovieMixInfo.PhysicalVideoFileFullPath, task_queue.DefaultTaskPriorityLevel,
			oneMovieMixInfo.VideoInfo.Id,
		))
		if err != nil {
			v.log.Errorln("filterMovieAndSeriesNeedDownloadEmby.Movie.NewOneJob", err)
			continue
		}
		if bok == false {
			v.log.Warningln(common.Movie.String(), oneMovieMixInfo.PhysicalVideoFileFullPath, "downloadQueue isExisted")
		}
	}
	// Emby 过滤，连续剧
	for _, embyMixInfos := range emby.SeriesSubNeedDlEmbyMixInfoMap {

		if len(embyMixInfos) < 1 {
			continue
		}

		// 只需要从一集取信息即可
		for _, mixInfo := range embyMixInfos {
			// 在 GetRecentlyAddVideoListWithNoChineseSubtitle 的时候就进行了筛选，所以这里就直接加入队列了
			// 放入队列
			oneJob := TTaskqueue.NewOneJob(
				common.Series, mixInfo.PhysicalVideoFileFullPath, task_queue.DefaultTaskPriorityLevel,
				mixInfo.VideoInfo.Id,
			)

			info, _, err := decode.GetVideoInfoFromFileFullPath(mixInfo.PhysicalVideoFileFullPath)
			if err != nil {
				v.log.Warningln("filterMovieAndSeriesNeedDownloadEmby.Series.GetVideoInfoFromFileFullPath", err)
				continue
			}
			oneJob.Season = info.Season
			oneJob.Episode = info.Episode
			oneJob.SeriesRootDirPath = mixInfo.PhysicalSeriesRootDir

			bok, err := v.downloadQueue.Add(*oneJob)
			if err != nil {
				v.log.Errorln("filterMovieAndSeriesNeedDownloadEmby.Series.NewOneJob", err)
				continue
			}
			if bok == false {
				v.log.Warningln(common.Series.String(), mixInfo.PhysicalVideoFileFullPath, "downloadQueue isExisted")
			}
		}
	}

	return nil
}

// getUpdateVideoListFromEmby 这里首先会进行近期影片的获取，然后对这些影片进行刷新，然后在获取字幕列表，最终得到需要字幕获取的 video 列表
func (v *VideoScanAndRefreshHelper) getUpdateVideoListFromEmby() ([]emby.EmbyMixInfo, map[string][]emby.EmbyMixInfo, error) {
	if v.embyHelper == nil {
		return nil, nil, nil
	}
	defer func() {
		v.log.Infoln("getUpdateVideoListFromEmby End")
	}()
	v.log.Infoln("getUpdateVideoListFromEmby Start...")
	//------------------------------------------------------
	var err error
	var movieList []emby.EmbyMixInfo
	var seriesSubNeedDlMap map[string][]emby.EmbyMixInfo //  多个需要搜索字幕的连续剧目录，连续剧文件夹名称 -- 每一集的 EmbyMixInfo List
	movieList, seriesSubNeedDlMap, err = v.embyHelper.GetRecentlyAddVideoListWithNoChineseSubtitle(v.NeedForcedScanAndDownSub)
	if err != nil {
		return nil, nil, err
	}
	// 输出调试信息
	v.log.Debugln("getUpdateVideoListFromEmby - DebugInfo - movieFileFullPathList Start")
	for _, info := range movieList {
		v.log.Debugln(info.PhysicalVideoFileFullPath)
	}
	v.log.Debugln("getUpdateVideoListFromEmby - DebugInfo - movieFileFullPathList End")

	v.log.Debugln("getUpdateVideoListFromEmby - DebugInfo - seriesSubNeedDlMap Start")
	for s := range seriesSubNeedDlMap {
		v.log.Debugln(s)
	}
	v.log.Debugln("getUpdateVideoListFromEmby - DebugInfo - seriesSubNeedDlMap End")

	return movieList, seriesSubNeedDlMap, nil
}

func (v *VideoScanAndRefreshHelper) RestoreFixTimelineBK() error {

	defer v.log.Infoln("End Restore Fix Timeline BK")
	v.log.Infoln("Start Restore Fix Timeline BK...")
	//------------------------------------------------------
	_, err := subTimelineFixerPKG.Restore(v.log, v.settings.CommonSettings.MoviePaths, v.settings.CommonSettings.SeriesPaths)
	if err != nil {
		return err
	}
	return nil
}

type ScanVideoResult struct {
	Normal *NormalScanVideoResult
	Emby   *EmbyScanVideoResult
}

type NormalScanVideoResult struct {
	MoviesDirMap *treemap.Map
	SeriesDirMap *treemap.Map
}

type EmbyScanVideoResult struct {
	MovieSubNeedDlEmbyMixInfoList []emby.EmbyMixInfo
	SeriesSubNeedDlEmbyMixInfoMap map[string][]emby.EmbyMixInfo
}

type TaskInputData struct {
	Index     int
	InputPath string
}
