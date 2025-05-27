package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/trembon/switch-library-manager/db"
	"github.com/trembon/switch-library-manager/process"
	"github.com/trembon/switch-library-manager/settings"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"go.uber.org/zap"
)

type Pair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type LocalLibraryData struct {
	LibraryData []LibraryTemplateData `json:"library_data"`
	Issues      []Pair                `json:"issues"`
	NumFiles    int                   `json:"num_files"`
}

type SwitchTitle struct {
	Name        string `json:"name"`
	TitleId     string `json:"titleId"`
	Icon        string `json:"icon"`
	Region      string `json:"region"`
	ReleaseDate string `json:"release_date"`
}

type LibraryTemplateData struct {
	Id      int    `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Dlc     string `json:"dlc"`
	TitleId string `json:"titleId"`
	Path    string `json:"path"`
	Icon    string `json:"icon"`
	Update  int    `json:"update"`
	Region  string `json:"region"`
	Type    string `json:"type"`
}

type ProgressUpdate struct {
	Curr    int    `json:"curr"`
	Total   int    `json:"total"`
	Message string `json:"message"`
}

type State struct {
	sync.Mutex
	switchDB *db.SwitchTitlesDB
	localDB  *db.LocalSwitchFilesDB
}

type GUI struct {
	ctx            context.Context
	state          State
	baseFolder     string
	localDbManager *db.LocalSwitchDBManager
	sugarLogger    *zap.SugaredLogger
}

var assets embed.FS

func CreateGUI(baseFolder string, sugarLogger *zap.SugaredLogger) *GUI {
	return &GUI{state: State{}, baseFolder: baseFolder, sugarLogger: sugarLogger}
}

// Startup is called when the app starts.
func (g *GUI) Startup(ctx context.Context) {
	g.ctx = ctx
	g.sugarLogger.Info("GUI Startup")

	var err error
	g.localDbManager, err = db.NewLocalSwitchDBManager(g.baseFolder)
	if err != nil {
		// Use Fatalf which will log and exit. Wails might not start properly.
		g.sugarLogger.Fatalf("Failed to create local files db: %v", err)
		return
	}

	_, keyErr := settings.InitSwitchKeys(g.baseFolder)
	if keyErr != nil {
		g.sugarLogger.Warnf("Failed to initialize switch keys: %v (this might be expected if keys are not present or error is non-critical)", keyErr)
	}
}

// Shutdown is called at application termination
func (g *GUI) Shutdown(ctx context.Context) {
	g.sugarLogger.Info("GUI Shutdown")
	if g.localDbManager != nil {
		g.localDbManager.Close()
	}
}

func (g *GUI) Start() {
	// Create Wails menu
	mainMenu := menu.NewMenu()
	if runtime.GOOS == "darwin" {
		mainMenu.Append(menu.AppMenu()) // App specific menu (About, Quit, etc.)
	}

	fileMenu := mainMenu.AddSubmenu("File")
	// "Scan Library" menu item - corrected to use wailsRuntime
	fileMenu.AddText("Scan Library", nil, func(_ *menu.CallbackData) {
		wailsRuntime.EventsEmit(g.ctx, "requestUpdateLocalLibrary", false)
	})
	fileMenu.AddText("Hard Rescan Library", nil, func(_ *menu.CallbackData) {
		go func() {
			g.state.Lock()
			cleared := false
			if g.localDbManager != nil {
				err := g.localDbManager.ClearScanData()
				if err != nil {
					g.sugarLogger.Errorf("Failed to clear scan data: %v", err)
					wailsRuntime.EventsEmit(g.ctx, "generalError", fmt.Sprintf("Failed to clear scan data: %v", err))
				} else {
					cleared = true
					g.sugarLogger.Info("Scan data cleared for hard rescan.")
				}
			} else {
				g.sugarLogger.Warn("Local DB manager not initialized for hard rescan.")
			}
			g.state.Unlock()
			// Proceed to request update even if clearing failed or manager was nil,
			// as the user still expects a refresh.
			if cleared || g.localDbManager == nil {
				wailsRuntime.EventsEmit(g.ctx, "requestUpdateLocalLibrary", true)
			}
		}()
	})

	if runtime.GOOS != "darwin" { // Add Quit to File menu for non-macOS
		fileMenu.AddSeparator()
		fileMenu.AddText("Quit", nil, func(_ *menu.CallbackData) {
			wailsRuntime.Quit(g.ctx)
		})
	}

	//mainMenu.Append(menu.EditMenu()) // Provides Copy, Paste, Cut, Select All

	//debugMenu := mainMenu.AddSubmenu("Debug")
	//debugMenu.AddText("Open DevTools", nil, func(_ *menu.CallbackData) {
	//	wails.OpenDevTools(g.ctx)
	//})
	err := wails.Run(&options.App{
		Title:  "Switch Library Manager (" + settings.SLM_VERSION + ")",
		Width:  1200, // Adjusted from original Astilectron/Wails sizes
		Height: 700,  // Adjusted
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		OnStartup:        g.Startup,
		OnShutdown:       g.Shutdown,
		Menu:             mainMenu,
		Bind: []interface{}{
			g,
		},
	})
	if err != nil {
		g.sugarLogger.Fatalf("Error running Wails app: %v", err)
	}
}

func getType(gameFile *db.SwitchGameFiles) string {
	if gameFile.IsSplit {
		return "split"
	}
	if gameFile.MultiContent {
		return "multi-content"
	}
	ext := filepath.Ext(gameFile.File.ExtendedInfo.FileName)
	if len(ext) > 1 {
		return ext[1:]
	}
	return ""
}

// --- Bound Go Methods ---

func (g *GUI) OrganizeLibrary() {
	g.state.Lock()
	defer g.state.Unlock()

	if g.state.localDB == nil || g.state.switchDB == nil {
		errStr := "Local or Switch DB not loaded. Please scan/update DB first."
		g.sugarLogger.Error(errStr)
		wailsRuntime.EventsEmit(g.ctx, "generalError", errStr)
		return
	}

	// Consider caching settings or passing AppSettings object to avoid multiple reads
	currentSettings := settings.ReadSettings(g.baseFolder)
	folderToScan := currentSettings.Folder
	organizeOptions := currentSettings.OrganizeOptions

	if !process.IsOptionsValid(organizeOptions) {
		errStr := "The organize options in settings.json are not valid, please check that the template contains file/folder name"
		g.sugarLogger.Error(errStr)
		wailsRuntime.EventsEmit(g.ctx, "generalError", errStr)
		return
	}
	process.OrganizeByFolders(folderToScan, g.state.localDB, g.state.switchDB, g) // g implements UpdateProgress
	if organizeOptions.DeleteOldUpdateFiles {
		process.DeleteOldUpdates(g.baseFolder, g.state.localDB, g) // g implements UpdateProgress
	}
	wailsRuntime.EventsEmit(g.ctx, "organizeCompleted")
	g.sugarLogger.Info("Library organization process completed.")
}

func (g *GUI) IsKeysFileAvailable() bool {
	// This method doesn't access shared g.state, so no lock needed here.
	keys, _ := settings.SwitchKeys() // SwitchKeys handles its own initialization logic
	return keys != nil && keys.GetKey("header_key") != ""
}

func (g *GUI) LoadSettings() string {
	// This method doesn't access shared g.state.
	// runtime.WindowSetAlwaysOnTop(g.ctx, false) // Removed: original intent unclear here. Manage from JS if needed.
	return settings.ReadSettingsAsJSON(g.baseFolder)
}

func (g *GUI) SaveSettings(settingsJson string) error {
	// This method doesn't access shared g.state.
	s := settings.AppSettings{}
	err := json.Unmarshal([]byte(settingsJson), &s)
	if err != nil {
		g.sugarLogger.Errorf("Failed to unmarshal settings: %v", err)
		return err
	}
	settings.SaveSettings(&s, g.baseFolder)
	g.sugarLogger.Info("Settings saved successfully.")
	return nil
}

func (g *GUI) GetMissingDLC() string {
	g.state.Lock()
	defer g.state.Unlock()

	if g.state.localDB == nil || g.state.switchDB == nil {
		g.sugarLogger.Warn("GetMissingDLC called before DBs are initialized.")
		wailsRuntime.EventsEmit(g.ctx, "generalError", "Databases not yet loaded. Please scan library first.")
		return "[]" // Return empty JSON array string
	}

	settingsObj := settings.ReadSettings(g.baseFolder)
	ignoreIds := map[string]struct{}{}
	for _, id := range settingsObj.IgnoreDLCTitleIds {
		ignoreIds[strings.ToLower(id)] = struct{}{}
	}
	missingDLC := process.ScanForMissingDLC(g.state.localDB.TitlesMap, g.state.switchDB.TitlesMap, ignoreIds)
	values := make([]process.IncompleteTitle, len(missingDLC))
	for _, missingUpdate := range missingDLC {
		values = append(values, missingUpdate)
	}

	msg, err := json.Marshal(values)
	if err != nil {
		g.sugarLogger.Errorf("Failed to marshal missing DLC: %v", err)
		wailsRuntime.EventsEmit(g.ctx, "generalError", "Error processing missing DLC.")
		return "[]"
	}
	return string(msg)
}

func (g *GUI) GetMissingUpdates() string {
	g.state.Lock()
	defer g.state.Unlock()

	if g.state.localDB == nil || g.state.switchDB == nil {
		g.sugarLogger.Warn("GetMissingUpdates called before DBs are initialized.")
		wailsRuntime.EventsEmit(g.ctx, "generalError", "Databases not yet loaded. Please scan library first.")
		return "[]"
	}

	settingsObj := settings.ReadSettings(g.baseFolder)
	ignoreIds := map[string]struct{}{}
	for _, id := range settingsObj.IgnoreUpdateTitleIds {
		ignoreIds[strings.ToLower(id)] = struct{}{}
	}
	missingUpdates := process.ScanForMissingUpdates(g.state.localDB.TitlesMap, g.state.switchDB.TitlesMap, ignoreIds, settingsObj.IgnoreDLCUpdates)
	values := make([]process.IncompleteTitle, 0, len(missingUpdates))
	for _, missingUpdate := range missingUpdates {
		values = append(values, missingUpdate)
	}

	msg, err := json.Marshal(values)
	if err != nil {
		g.sugarLogger.Errorf("Failed to marshal missing updates: %v", err)
		wailsRuntime.EventsEmit(g.ctx, "generalError", "Error processing missing updates.")
		return "[]"
	}
	return string(msg)
}

//func (g *GUI) loadSettings() string {
//	return settings.ReadSettingsAsJSON(g.baseFolder)
//}

func (g *GUI) UpdateLocalLibrary(ignoreCache bool) {
	g.state.Lock()
	defer g.state.Unlock()

	if g.localDbManager == nil {
		errMsg := "Local DB manager not initialized. Cannot update local library."
		g.sugarLogger.Error(errMsg)
		wailsRuntime.EventsEmit(g.ctx, "generalError", errMsg)
		return
	}

	localDB, err := g.buildLocalDB(g.localDbManager, ignoreCache)
	if err != nil {
		g.sugarLogger.Errorf("Failed to build local DB: %v", err)
		wailsRuntime.EventsEmit(g.ctx, "generalError", fmt.Sprintf("Error updating local library: %v", err))
		return
	}
	g.state.localDB = localDB // Update the shared state

	if g.state.switchDB == nil {
		g.sugarLogger.Warn("Switch DB not loaded during UpdateLocalLibrary. Some title information might be incomplete.")
		// Optionally emit an event to frontend to suggest updating Switch DB
	}

	response := LocalLibraryData{}
	libraryData := []LibraryTemplateData{}
	issues := []Pair{}

	for titleID, gameFiles := range g.state.localDB.TitlesMap { // Use the updated g.state.localDB
		if gameFiles.BaseExist {
			version := ""
			name := ""
			iconURL := ""
			region := ""

			if gameFiles.File.Metadata.Ncap != nil {
				version = gameFiles.File.Metadata.Ncap.DisplayVersion
				name = gameFiles.File.Metadata.Ncap.TitleName["AmericanEnglish"].Title
			}

			if len(gameFiles.Updates) > 0 {
				latestUpdateFile := gameFiles.Updates[gameFiles.LatestUpdate]
				if latestUpdateFile.Metadata.Ncap != nil {
					version = latestUpdateFile.Metadata.Ncap.DisplayVersion
				}
			}

			if g.state.switchDB != nil {
				if title, ok := g.state.switchDB.TitlesMap[titleID]; ok {
					if title.Attributes.Name != "" {
						name = title.Attributes.Name
					}
					iconURL = title.Attributes.IconUrl
					region = title.Attributes.Region
				}
			}

			if name == "" { // Fallback name
				name = db.ParseTitleNameFromFileName(gameFiles.File.ExtendedInfo.FileName)
			}

			libraryData = append(libraryData,
				LibraryTemplateData{
					Icon:    iconURL,
					Name:    name,
					TitleId: titleID,
					Update:  gameFiles.LatestUpdate,
					Version: version,
					Region:  region,
					Type:    getType(gameFiles),
					Path:    filepath.Join(gameFiles.File.ExtendedInfo.BaseFolder, gameFiles.File.ExtendedInfo.FileName),
				})

		} else { // Base does not exist, list associated files as issues
			for _, update := range gameFiles.Updates {
				issues = append(issues, Pair{Key: filepath.Join(update.ExtendedInfo.BaseFolder, update.ExtendedInfo.FileName), Value: "Base file is missing"})
			}
			for _, dlc := range gameFiles.Dlc {
				issues = append(issues, Pair{Key: filepath.Join(dlc.ExtendedInfo.BaseFolder, dlc.ExtendedInfo.FileName), Value: "Base file is missing"})
			}
		}
	}
	for k, v := range g.state.localDB.Skipped { // Use the updated g.state.localDB
		issues = append(issues, Pair{Key: filepath.Join(k.BaseFolder, k.FileName), Value: v.ReasonText})
	}

	response.LibraryData = libraryData
	response.NumFiles = g.state.localDB.NumFiles
	response.Issues = issues

	wailsRuntime.EventsEmit(g.ctx, "libraryLoaded", response)
	g.sugarLogger.Infof("Local library updated. %d files processed.", response.NumFiles)
}

func (g *GUI) buildSwitchDb() (*db.SwitchTitlesDB, error) {
	settingsObj := settings.ReadSettings(g.baseFolder)
	//1. load the titles JSON object
	g.UpdateProgress(1, 4, "Downloading titles.json")
	filename := filepath.Join(g.baseFolder, settings.TITLE_JSON_FILENAME)
	titleFile, titlesEtag, err := db.LoadAndUpdateFile(settingsObj.TitlesJsonUrl, filename, settingsObj.TitlesEtag)
	if err != nil {
		return nil, errors.New("failed to download switch titles [reason:" + err.Error() + "]")
	}
	settingsObj.TitlesEtag = titlesEtag

	g.UpdateProgress(2, 4, "Downloading versions.json")
	filename = filepath.Join(g.baseFolder, settings.VERSIONS_JSON_FILENAME)
	versionsFile, versionsEtag, err := db.LoadAndUpdateFile(settingsObj.VersionsJsonUrl, filename, settingsObj.VersionsEtag)
	if err != nil {
		return nil, errors.New("failed to download switch updates [reason:" + err.Error() + "]")
	}
	settingsObj.VersionsEtag = versionsEtag

	settings.SaveSettings(settingsObj, g.baseFolder)

	g.UpdateProgress(3, 4, "Processing switch titles and updates ...")
	switchTitleDB, err := db.CreateSwitchTitleDB(titleFile, versionsFile)
	g.UpdateProgress(4, 4, "Finishing up...")
	return switchTitleDB, err
}

func (g *GUI) UpdateDB() {
	switchDb, err := g.buildSwitchDb()
	if err != nil {
		g.sugarLogger.Errorf("Failed to build switch DB: %v", err)
		wailsRuntime.EventsEmit(g.ctx, "generalError", fmt.Sprintf("Error updating switch titles database: %v", err))
		return
	}

	g.state.Lock()
	g.state.switchDB = switchDb
	g.state.Unlock()

	g.sugarLogger.Info("Switch titles database updated successfully.")
	wailsRuntime.EventsEmit(g.ctx, "switchDBUpdated")
}

func (g *GUI) CheckUpdate() bool {
	newUpdate, err := settings.CheckForUpdates()
	if err != nil {
		g.sugarLogger.Warnf("Failed to check for SLM updates: %v", err)
		// Avoid sending error for common network issues like "dial tcp"
		if !strings.Contains(err.Error(), "dial tcp") && !strings.Contains(err.Error(), "no such host") {
			wailsRuntime.EventsEmit(g.ctx, "generalError", fmt.Sprintf("Error checking for application updates: %v", err))
		}
		return false // Return false on error
	}
	if newUpdate {
		g.sugarLogger.Info("New application version available.")
	} else {
		g.sugarLogger.Info("Application is up to date.")
	}
	return newUpdate
}

// buildLocalDB is a helper, not directly bound. It's called by UpdateLocalLibrary.
// The caller (UpdateLocalLibrary) handles g.state.Lock and updating g.state.localDB.
func (g *GUI) buildLocalDB(localDbManager *db.LocalSwitchDBManager, ignoreCache bool) (*db.LocalSwitchFilesDB, error) {
	folderToScan := settings.ReadSettings(g.baseFolder).Folder
	recursiveMode := settings.ReadSettings(g.baseFolder).ScanRecursively
	scanFolders := settings.ReadSettings(g.baseFolder).ScanFolders
	scanFolders = append(scanFolders, folderToScan)
	localDB, err := localDbManager.CreateLocalSwitchFilesDB(scanFolders, g, recursiveMode, ignoreCache)
	return localDB, err
}

func (g *GUI) UpdateProgress(curr int, total int, message string) {
	if g.ctx == nil {
		// This can happen if UpdateProgress is called before Startup completes or after Shutdown.
		g.sugarLogger.Warnf("UpdateProgress called with nil context (message: %s). GUI might not be fully initialized or is shutting down.", message)
		return
	}
	progressMessage := ProgressUpdate{curr, total, message}
	g.sugarLogger.Debugf("%v (%v/%v)", message, curr, total)
	wailsRuntime.EventsEmit(g.ctx, "updateProgress", progressMessage)
}

func (g *GUI) GetMissingGames() []SwitchTitle {
	g.state.Lock()
	defer g.state.Unlock()

	if g.state.localDB == nil || g.state.switchDB == nil {
		g.sugarLogger.Warn("GetMissingGames called before DBs are initialized.")
		wailsRuntime.EventsEmit(g.ctx, "generalError", "Databases not yet loaded. Please scan library first.")
		return []SwitchTitle{}
	}

	var result []SwitchTitle
	options := settings.ReadSettings(g.baseFolder) // Read settings once for this operation

	for k, v := range g.state.switchDB.TitlesMap {
		if _, ok := g.state.localDB.TitlesMap[k]; ok {
			continue
		}
		if v.Attributes.Name == "" || v.Attributes.Id == "" {
			continue
		}

		if options.HideDemoGames && v.Attributes.IsDemo {
			continue
		}
		result = append(result, SwitchTitle{
			TitleId:     v.Attributes.Id,
			Name:        v.Attributes.Name,
			Icon:        v.Attributes.BannerUrl,
			Region:      v.Attributes.Region,
			ReleaseDate: v.Attributes.ParsedReleaseDate,
		})
	}
	return result

}
