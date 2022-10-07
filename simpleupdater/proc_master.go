package simpleupdater

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zzzhr1990/simple-updater/model"
)

// a simpleupdater master process
type master struct {
	*Config
	slaveID             int
	slaveCmd            *exec.Cmd
	slaveExtraFiles     []*os.File
	binPath             string
	binDirectory        string
	binTempDirectory    string
	binPerms            os.FileMode
	binHash             []byte
	restarting          bool
	restartedAt         time.Time
	restarted           chan bool
	awaitingUSR1        bool
	descriptorsReleased chan bool
	signalledAt         time.Time
	printCheckUpdate    bool
	runningVersion      map[string]*model.VersionInfo
	lastCheckTime       time.Time
}

func newMaster(config *Config) *master {
	return &master{
		Config:         config,
		runningVersion: make(map[string]*model.VersionInfo),
	}
}

func (mp *master) run() error {
	mp.debugf("run")
	if err := mp.checkBinary(); err != nil {
		return err
	}
	if mp.Config.Fetcher != nil {
		if err := mp.Config.Fetcher.Init(); err != nil {
			mp.warnf("fetcher init failed (%s). fetcher disabled.", err)
			mp.Config.Fetcher = nil
		}
	}
	mp.setupSignalling()
	if err := mp.retreiveFileDescriptors(); err != nil {
		return err
	}
	if mp.Config.Fetcher != nil {
		mp.printCheckUpdate = true
		mp.fetch()
		go mp.fetchLoop()
	}
	return mp.forkLoop()
}

func (mp *master) checkBinary() error {
	//get path to binary and confirm its writable
	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find binary path (%s)", err)
	}
	mp.binPath = binPath
	if info, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("failed to stat binary (%s)", err)
	} else if info.Size() == 0 {
		return fmt.Errorf("binary file is empty")
	} else {
		//copy permissions
		mp.binPerms = info.Mode()
	}

	//get directory of binary
	binDirectory := filepath.Dir(binPath)
	mp.binDirectory = binDirectory
	tempDirectory := filepath.Join(binDirectory, "_updatetemp")
	mp.binTempDirectory = tempDirectory
	os.RemoveAll(tempDirectory)
	err = os.MkdirAll(tempDirectory, 0755)
	if err != nil {
		return fmt.Errorf("failed to create temp directory (%s)", err)
	}
	tmpBinPath := filepath.Join(tempDirectory, "simpleupdater-"+token()+extension())
	f, err := os.Open(binPath)
	if err != nil {
		return fmt.Errorf("cannot read binary (%s)", err)
	}

	//initial hash of file
	hash := sha1.New()
	io.Copy(hash, f)
	mp.binHash = hash.Sum(nil)
	f.Close()
	//test bin<->tmpbin moves
	if mp.Config.Fetcher != nil {
		if err := move(tmpBinPath, mp.binPath); err != nil {
			return fmt.Errorf("cannot move binary (%s)", err)
		}
		if err := move(mp.binPath, tmpBinPath); err != nil {
			return fmt.Errorf("cannot move binary back (%s)", err)
		}
	}
	return nil
}

func (mp *master) setupSignalling() {
	//updater-forker comms
	mp.restarted = make(chan bool)
	mp.descriptorsReleased = make(chan bool)
	//read all master process signals
	signals := make(chan os.Signal)
	signal.Notify(signals)
	go func() {
		for s := range signals {
			mp.handleSignal(s)
		}
	}()
}

func (mp *master) handleSignal(s os.Signal) {
	if s == mp.RestartSignal {
		//user initiated manual restart
		go mp.triggerRestart()
	} else if s.String() == "child exited" {
		// will occur on every restart, ignore it
	} else
	//**during a restart** a SIGUSR1 signals
	//to the master process that, the file
	//descriptors have been released
	if mp.awaitingUSR1 && s == SIGUSR1 {
		mp.debugf("signaled, sockets ready")
		mp.awaitingUSR1 = false
		mp.descriptorsReleased <- true
	} else
	//while the slave process is running, proxy
	//all signals through
	if mp.slaveCmd != nil && mp.slaveCmd.Process != nil {
		mp.debugf("proxy signal (%s)", s)
		mp.sendSignal(s)
	} else
	//otherwise if not running, kill on CTRL+c
	if s == os.Interrupt {
		mp.debugf("interupt with no slave")
		os.Exit(1)
	} else {
		mp.debugf("signal discarded (%s), no slave process", s)
	}
}

func (mp *master) sendSignal(s os.Signal) {
	if mp.slaveCmd != nil && mp.slaveCmd.Process != nil {
		if err := mp.slaveCmd.Process.Signal(s); err != nil {
			mp.debugf("signal failed (%s), assuming slave process died unexpectedly", err)
			os.Exit(1)
		}
	}
}

func (mp *master) retreiveFileDescriptors() error {
	mp.slaveExtraFiles = make([]*os.File, len(mp.Config.Addresses))
	for i, addr := range mp.Config.Addresses {
		a, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			return fmt.Errorf("invalid address %s (%s)", addr, err)
		}
		l, err := net.ListenTCP("tcp", a)
		if err != nil {
			return err
		}
		f, err := l.File()
		if err != nil {
			return fmt.Errorf("failed to retreive fd for: %s (%s)", addr, err)
		}
		if err := l.Close(); err != nil {
			return fmt.Errorf("failed to close listener for: %s (%s)", addr, err)
		}
		mp.slaveExtraFiles[i] = f
	}
	return nil
}

// fetchLoop is run in a goroutine
func (mp *master) fetchLoop() {
	min := mp.Config.MinFetchInterval
	time.Sleep(min)
	for {
		t0 := time.Now()
		mp.fetch()
		//duration fetch of fetch
		diff := time.Since(t0)
		if diff < min {
			delay := min - diff
			//ensures at least MinFetchInterval delay.
			//should be throttled by the fetcher!
			time.Sleep(delay)
		}
	}
}

func (mp *master) updateAssets(updateInfo *model.UpdateCollection) bool {
	updated := false

	ex, err := os.Executable()
	if err != nil {
		mp.debugf("cannot determine executable path (%s)", err)
		return false
	}
	execPath := filepath.Dir(ex)

	for _, updateSingleAsset := range updateInfo.List {
		rawPath := strings.Split(updateSingleAsset.Path, "/")
		// fliter out the empty element
		rawPathClean := make([]string, 0)
		for _, v := range rawPath {
			vx := strings.TrimSpace(v)
			if len(vx) > 0 {
				rawPathClean = append(rawPathClean, vx)
			}
		}
		realFilePath := filepath.Join(execPath, filepath.Join(rawPathClean...))
		fileVersionKey := realFilePath
		mAssectVersion := mp.runningVersion[fileVersionKey]
		needDoUpdate := false
		if mAssectVersion == nil {
			// not calculate the hash
			f, err := os.Open(realFilePath)
			if err != nil {
				mp.debugf("cannot open file (%s)", err)
				needDoUpdate = true
			} else {
				hash := sha1.New()
				io.Copy(hash, f)
				assectHash := hash.Sum(nil)
				f.Close()
				hexHash := hex.EncodeToString(assectHash)
				if hexHash != updateSingleAsset.SHA1 {
					needDoUpdate = true
				} else {
					mp.runningVersion[fileVersionKey] = &model.VersionInfo{
						SHA1:    hexHash,
						Version: updateSingleAsset.Version,
					}
				}
			}
			//initial hash of file

			// mp.runningVersion[fileVersionKey] = mAssectVersion
		} else {
			if mAssectVersion.SHA1 != updateSingleAsset.SHA1 {
				needDoUpdate = true
				mp.debugf("sha not match, bump %s file (%s --> %s)", realFilePath, mAssectVersion.Version, updateSingleAsset.Version)
			} else {
				if mAssectVersion.Version != updateSingleAsset.Version {
					mp.debugf("version bump %s file (%s --> %s)", realFilePath, mAssectVersion.Version, updateSingleAsset.Version)
					mAssectVersion.Version = updateSingleAsset.Version
				}
			}
		}
		if needDoUpdate {
			fileTempName := updateSingleAsset.SHA1
			if len(fileTempName) < 1 {
				fileTempName = "simpleupdater-" + token() + ".tmp"
			}
			tempfile := filepath.Join(mp.binTempDirectory, fileTempName)
			mp.debugf("updating assect file %s", realFilePath)
			reader, err := mp.Config.Fetcher.GetUpdateStream(updateSingleAsset.URL)
			if err != nil {
				mp.debugf("failed to fetch update (%s)", err)
				return false
			}

			if reader == nil {
				if mp.printCheckUpdate {
					mp.debugf("no updates")
				}
				return false
			}
			mp.debugf("streaming update...")
			//optional closer
			if closer, ok := reader.(io.Closer); ok {
				defer closer.Close()
			}
			tmpBin, err := os.OpenFile(tempfile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
			if err != nil {
				mp.warnf("failed to open temp file: %s", err)
				return false
			}
			defer func() {
				tmpBin.Close()
				os.Remove(tempfile)
			}()
			//tee off to sha1
			hash := sha1.New()
			reader = io.TeeReader(reader, hash)
			//write to a temp file
			_, err = io.Copy(tmpBin, reader)
			if err != nil {
				mp.warnf("failed to write temp file: %s", err)
				return false
			}
			//compare hash
			newHash := hash.Sum(nil)
			if bytes.Equal(mp.binHash, newHash) {
				mp.debugf("hash match - skip")
				hexHash := hex.EncodeToString(newHash)
				mp.runningVersion[fileVersionKey] = &model.VersionInfo{
					SHA1:    hexHash,
					Version: updateSingleAsset.Version,
				}
				continue
			}

			newHashHex := hex.EncodeToString(newHash)
			if newHashHex != updateSingleAsset.SHA1 {
				mp.warnf("hash mismatch - expected %s, got %s", updateSingleAsset.SHA1, newHashHex)
				return false
			}

			//copy permissions
			if err := chmod(tmpBin, mp.binPerms); err != nil {
				mp.warnf("failed to make temp binary executable: %s", err)
				return false
			}
			if err := chown(tmpBin, uid, gid); err != nil {
				mp.warnf("failed to change owner of binary: %s", err)
				return false
			}
			if _, err := tmpBin.Stat(); err != nil {
				mp.warnf("failed to stat temp binary: %s", err)
				return false
			}
			tmpBin.Close()
			if _, err := os.Stat(tempfile); err != nil {
				mp.warnf("failed to stat temp binary by path: %s", err)
				return false
			}
			//overwrite!
			frp := filepath.Dir(realFilePath)
			if _, err := os.Stat(frp); err != nil {
				os.MkdirAll(frp, 0755)
			}
			if err := overwrite(realFilePath, tempfile); err != nil {
				mp.warnf("failed to overwrite binary: %s", err)
				return false
			}
			mp.debugf("upgraded assect : %s (%s -> %s)", realFilePath, mAssectVersion.Version, updateSingleAsset.Version)
			mp.runningVersion[fileVersionKey] = &model.VersionInfo{
				SHA1:    newHashHex,
				Version: updateSingleAsset.Version,
			}
			updated = true
		}
	}
	return updated
}

func (mp *master) updateMain(updateInfo *model.UpdateCollection) bool {
	mExecutableVersionKey := ":mExecutable"
	mExecutableVersion := mp.runningVersion[mExecutableVersionKey]
	currentHash := hex.EncodeToString(mp.binHash)
	if currentHash == updateInfo.Main.SHA1 {
		mp.debugf("no update available")
		if mExecutableVersion == nil || mExecutableVersion.Version != updateInfo.Main.Version {
			mp.debugf("updating version to %s", updateInfo.Main.Version)
			mp.runningVersion[mExecutableVersionKey] = &model.VersionInfo{
				Version: updateInfo.Main.Version,
				SHA1:    currentHash,
			}
		}
		return false
	}
	// do real update
	mp.debugf("updating main executable")
	reader, err := mp.Config.Fetcher.GetUpdateStream(updateInfo.Main.URL)
	if err != nil {
		mp.debugf("failed to fetch update (%s)", err)
		return false
	}

	if reader == nil {
		if mp.printCheckUpdate {
			mp.debugf("no updates")
		}
		mp.printCheckUpdate = false
		return false
	}
	mp.printCheckUpdate = true
	mp.debugf("streaming update...")
	//optional closer
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}
	fileTempName := updateInfo.Main.SHA1
	if len(fileTempName) < 1 {
		fileTempName = "simpleupdater-" + token() + extension()
	}
	tmpBinPath := filepath.Join(mp.binTempDirectory, fileTempName)
	tmpBin, err := os.OpenFile(tmpBinPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		mp.warnf("failed to open temp binary: %s", err)
		return false
	}
	defer func() {
		tmpBin.Close()
		os.Remove(tmpBinPath)
	}()
	//tee off to sha1
	hash := sha1.New()
	reader = io.TeeReader(reader, hash)
	//write to a temp file
	_, err = io.Copy(tmpBin, reader)
	if err != nil {
		mp.warnf("failed to write temp binary: %s", err)
		return false
	}
	//compare hash
	newHash := hash.Sum(nil)
	if bytes.Equal(mp.binHash, newHash) {
		mp.debugf("hash match - skip")
		skHash := hex.EncodeToString(newHash)
		mp.runningVersion[mExecutableVersionKey] = &model.VersionInfo{
			Version: updateInfo.Main.Version,
			SHA1:    skHash,
		}
		return false
	}

	newHashHex := hex.EncodeToString(newHash)
	if newHashHex != updateInfo.Main.SHA1 {
		mp.warnf("hash mismatch - expected %s, got %s", updateInfo.Main.SHA1, newHashHex)
		return false
	}

	//copy permissions
	if err := chmod(tmpBin, mp.binPerms); err != nil {
		mp.warnf("failed to make temp binary executable: %s", err)
		return false
	}
	if err := chown(tmpBin, uid, gid); err != nil {
		mp.warnf("failed to change owner of binary: %s", err)
		return false
	}
	if _, err := tmpBin.Stat(); err != nil {
		mp.warnf("failed to stat temp binary: %s", err)
		return false
	}
	tmpBin.Close()
	if _, err := os.Stat(tmpBinPath); err != nil {
		mp.warnf("failed to stat temp binary by path: %s", err)
		return false
	}
	if mp.Config.PreUpgrade != nil {
		if err := mp.Config.PreUpgrade(tmpBinPath); err != nil {
			mp.warnf("user cancelled upgrade: %s", err)
			return false
		}
	}
	//simpleupdater sanity check, dont replace our good binary with a non-executable file
	tokenIn := token()
	cmd := exec.Command(tmpBinPath)
	cmd.Env = append(os.Environ(), []string{envBinCheck + "=" + tokenIn}...)
	cmd.Args = os.Args
	returned := false
	go func() {
		time.Sleep(5 * time.Second)
		if !returned {
			mp.warnf("sanity check against fetched executable timed-out, check simpleupdater is running")
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		}
	}()
	tokenOut, err := cmd.CombinedOutput()
	returned = true
	if err != nil {
		mp.warnf("failed to run temp binary: %s (%s) output \"%s\"", err, tmpBinPath, tokenOut)
		return false
	}
	if tokenIn != string(tokenOut) {
		mp.warnf("sanity check failed")
		return false
	}
	//overwrite!
	if err := overwrite(mp.binPath, tmpBinPath); err != nil {
		mp.warnf("failed to overwrite binary: %s", err)
		return false
	}
	mp.debugf("upgraded binary (%x -> %x)", mp.binHash[:12], newHash[:12])
	mp.binHash = newHash
	mp.runningVersion[mExecutableVersionKey] = &model.VersionInfo{
		Version: updateInfo.Main.Version,
		SHA1:    newHashHex,
	} // updateInfo.Main.Version
	return true
}

func (mp *master) fetch() {
	if mp.restarting {
		return //skip if restarting
	}
	if mp.printCheckUpdate {
		mp.debugf("checking for updates...")
	}

	if time.Since(mp.lastCheckTime) < time.Second*10 {
		return
	}
	updateInfo, err := mp.Config.Fetcher.GetUpdateInfo(mp.Config.Channel, mp.Config.Name, mp.Config.CurrentVersion)
	if err != nil {
		mp.debugf("failed to fetch update info (%s)", err)
		return
	}

	if updateInfo == nil {
		mp.debugf("no update available")
		return
	}

	if updateInfo.Type == "binary" {
		mp.debugf("update turn binary mode (%s)", updateInfo.Version)
	} else {
		mp.debugf("update turn unknown mode (%s), version: %s", updateInfo.Type, updateInfo.Version)
		return
	}

	fileUpdated := mp.updateMain(updateInfo)
	assectsUpdated := mp.updateAssets(updateInfo)

	updated := fileUpdated || assectsUpdated
	if !updated {
		return
	}

	// 1. update main binary

	//binary successfully replaced
	if !mp.Config.NoRestartAfterFetch {
		mp.triggerRestart()
	}
	//and keep fetching...
	mp.lastCheckTime = time.Now()

}

func (mp *master) triggerRestart() {
	if mp.restarting {
		mp.debugf("already graceful restarting")
		return //skip
	} else if mp.slaveCmd == nil || mp.restarting {
		mp.debugf("no slave process")
		return //skip
	}
	mp.debugf("graceful restart triggered")
	mp.restarting = true
	mp.awaitingUSR1 = true
	mp.signalledAt = time.Now()
	mp.sendSignal(mp.Config.RestartSignal) //ask nicely to terminate
	select {
	case <-mp.restarted:
		//success
		mp.debugf("restart success")
	case <-time.After(mp.TerminateTimeout):
		//times up mr. process, we did ask nicely!
		mp.debugf("graceful timeout, forcing exit")
		mp.sendSignal(os.Kill)
	}
}

// not a real fork
func (mp *master) forkLoop() error {
	//loop, restart command
	for {
		if err := mp.fork(); err != nil {
			return err
		}
	}
}

func (mp *master) fork() error {
	mp.debugf("starting %s", mp.binPath)
	cmd := exec.Command(mp.binPath)
	//mark this new process as the "active" slave process.
	//this process is assumed to be holding the socket files.
	mp.slaveCmd = cmd
	mp.slaveID++
	//provide the slave process with some state
	e := os.Environ()
	e = append(e, envBinID+"="+hex.EncodeToString(mp.binHash))
	e = append(e, envBinPath+"="+mp.binPath)
	e = append(e, envSlaveID+"="+strconv.Itoa(mp.slaveID))
	e = append(e, envIsSlave+"=1")
	e = append(e, envNumFDs+"="+strconv.Itoa(len(mp.slaveExtraFiles)))
	cmd.Env = e
	//inherit master args/stdfiles
	cmd.Args = os.Args
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	//include socket files
	cmd.ExtraFiles = mp.slaveExtraFiles
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start slave process: %s", err)
	}
	//was scheduled to restart, notify success
	if mp.restarting {
		mp.restartedAt = time.Now()
		mp.restarting = false
		mp.restarted <- true
	}
	//convert wait into channel
	cmdwait := make(chan error)
	go func() {
		cmdwait <- cmd.Wait()
	}()
	//wait....
	select {
	case err := <-cmdwait:
		//program exited before releasing descriptors
		//proxy exit code out to master
		code := 0
		if err != nil {
			code = 1
			if exiterr, ok := err.(*exec.ExitError); ok {
				if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
					code = status.ExitStatus()
				}
			}
		}
		mp.debugf("prog exited with %d", code)
		//if a restarts are disabled or if it was an
		//unexpected crash, proxy this exit straight
		//through to the main process
		if mp.NoRestart || !mp.restarting {
			os.Exit(code)
		}
	case <-mp.descriptorsReleased:
		//if descriptors are released, the program
		//has yielded control of its sockets and
		//a parallel instance of the program can be
		//started safely. it should serve state.Listeners
		//to ensure downtime is kept at <1sec. The previous
		//cmd.Wait() will still be consumed though the
		//result will be discarded.
	}
	return nil
}

func (mp *master) debugf(f string, args ...interface{}) {
	if mp.Config.Debug {
		log.Printf("[simpleupdater master] "+f, args...)
	}
}

func (mp *master) warnf(f string, args ...interface{}) {
	if mp.Config.Debug || !mp.Config.NoWarn {
		log.Printf("[simpleupdater master] "+f, args...)
	}
}

func token() string {
	buff := make([]byte, 8)
	rand.Read(buff)
	return hex.EncodeToString(buff)
}

// On Windows, include the .exe extension, noop otherwise.
func extension() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}

	return ""
}
