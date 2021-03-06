// Copyright (c) 2013 Uwe Hoffmann. All rights reserved.

/*
Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google Inc. nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
*/

package archive

import (
	"bufio"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/golang/glog"
	"github.com/klauspost/compress/gzip"
	"github.com/uwedeportivo/romba/config"
	"github.com/uwedeportivo/romba/dedup"
	"github.com/uwedeportivo/romba/types"
	"github.com/uwedeportivo/torrentzip"
)

type gameBuilder struct {
	depot   *Depot
	datPath string
	fixDat  *types.Dat
	mutex   *sync.Mutex
	wc      chan *types.Game
	erc     chan error
	closeC  chan bool
	index   int
	deduper dedup.Deduper
}

func (gb *gameBuilder) work() {
	glog.V(4).Infof("starting subworker %d", gb.index)
	for game := range gb.wc {
		gamePath := filepath.Join(gb.datPath, game.Name)
		fixGame, foundRom, err := gb.depot.buildGame(game, gamePath, gb.fixDat.UnzipGames, gb.deduper)
		if err != nil {
			glog.Errorf("error processing %s: %v", gamePath, err)
			gb.erc <- err
			break
		}
		if fixGame != nil {
			gb.mutex.Lock()
			gb.fixDat.Games = append(gb.fixDat.Games, fixGame)
			gb.mutex.Unlock()
		}
		if !foundRom {
			if gb.fixDat.UnzipGames {
				err := os.RemoveAll(gamePath)
				if err != nil && !os.IsNotExist(err) {
					glog.Errorf("error removing %s: %v", gamePath, err)
					gb.erc <- err
					break
				}
			} else {
				err := os.Remove(gamePath + zipSuffix)
				if err != nil && !os.IsNotExist(err) {
					glog.Errorf("error removing %s: %v", gamePath+zipSuffix, err)
					gb.erc <- err
					break
				}
			}
		}
	}
	gb.closeC <- true
	glog.V(4).Infof("exiting subworker %d", gb.index)
	return
}

func (depot *Depot) BuildDat(dat *types.Dat, outpath string, numSubworkers int, deduper dedup.Deduper) (bool, error) {

	datPath := filepath.Join(outpath, dat.Name)

	err := os.Mkdir(datPath, 0777)
	if err != nil {
		return false, err
	}

	fixDat := new(types.Dat)
	fixDat.FixDat = true
	fixDat.Name = "fix_" + dat.Name
	fixDat.Description = dat.Description
	fixDat.Path = dat.Path
	fixDat.UnzipGames = dat.UnzipGames

	wc := make(chan *types.Game)
	erc := make(chan error)
	closeC := make(chan bool)
	mutex := new(sync.Mutex)

	for i := 0; i < numSubworkers; i++ {
		gb := new(gameBuilder)
		gb.depot = depot
		gb.wc = wc
		gb.erc = erc
		gb.mutex = mutex
		gb.datPath = datPath
		gb.fixDat = fixDat
		gb.index = i
		gb.deduper = deduper
		gb.closeC = closeC

		go gb.work()
	}

	var minionErr error

endLoop:
	for _, game := range dat.Games {
		select {
		case wc <- game:
		case err := <-erc:
			minionErr = err
			break endLoop
		}
	}
	close(wc)

	finishedSubworkers := 0

endLoop2:
	for {
		glog.V(4).Infof("builder master: finished so far %d", finishedSubworkers)

		select {
		case <-closeC:
			glog.V(4).Infof("builder master: finished another subworker")
			finishedSubworkers++
			if finishedSubworkers == numSubworkers {
				break endLoop2
			}
		case err := <-erc:
			glog.V(4).Infof("builder master: minion error")
			minionErr = err
		}
	}

	if minionErr != nil {
		return false, minionErr
	}

	if len(fixDat.Games) > 0 {
		fixDatPath := filepath.Join(outpath, fixPrefix+dat.Name+datSuffix)

		fixFile, err := os.Create(fixDatPath)
		if err != nil {
			return false, err
		}
		defer fixFile.Close()

		fixWriter := bufio.NewWriter(fixFile)
		defer fixWriter.Flush()

		err = types.ComposeCompliantDat(fixDat, fixWriter)
		if err != nil {
			return false, err
		}
	}

	return len(fixDat.Games) > 0, nil
}

type nopWriterCloser struct {
	io.Writer
}

func (nopWriterCloser) Close() error { return nil }

func (depot *Depot) buildGame(game *types.Game, gamePath string,
	unzipGame bool, deduper dedup.Deduper) (*types.Game, bool, error) {

	var gameTorrent *torrentzip.Writer
	var err error

	glog.V(4).Infof("building game %s with path %s", game.Name, gamePath)

	if unzipGame {
		err := os.Mkdir(gamePath, 0777)
		if err != nil {
			glog.Errorf("error mkdir %s: %v", gamePath, err)
			return nil, false, err
		}
	} else {
		gameDir := filepath.Dir(game.Name)
		if gameDir != "." {
			// name has dirs in it
			err := os.MkdirAll(filepath.Dir(gamePath), 0777)
			if err != nil {
				glog.Errorf("error mkdir %s: %v", filepath.Dir(gamePath), err)
				return nil, false, err
			}
		}

		gameFile, err := os.Create(gamePath + zipSuffix)
		if err != nil {
			glog.Errorf("error creating zip file %s: %v", gamePath+zipSuffix, err)
			return nil, false, err
		}
		defer gameFile.Close()

		gameTorrent, err = torrentzip.NewWriterWithTemp(gameFile, config.GlobalConfig.General.TmpDir)
		if err != nil {
			glog.Errorf("error writing to torrentzip file %s: %v", gamePath+zipSuffix, err)
			return nil, false, err
		}
		defer gameTorrent.Close()
	}

	var fixGame *types.Game

	foundRom := false

	for _, rom := range game.Roms {
		err = depot.romDB.CompleteRom(rom)
		if err != nil {
			glog.Errorf("error completing rom %s: %v", rom.Name, err)
			return nil, false, err
		}

		if rom.Sha1 == nil && rom.Size > 0 {
			if fixGame == nil {
				fixGame = new(types.Game)
				fixGame.Name = game.Name
				fixGame.Description = game.Description
			}

			fixGame.Roms = append(fixGame.Roms, rom)
			continue
		}

		romGZ, err := depot.OpenRomGZ(rom)
		if err != nil {
			glog.Errorf("error opening rom %s from depot: %v", rom.Name, err)
			return nil, false, err
		}

		if romGZ == nil {
			if glog.V(2) {
				glog.Warningf("game %s has missing rom %s (sha1 %s)", game.Name, rom.Name, hex.EncodeToString(rom.Sha1))
			}

			seenRom, err := deduper.Seen(rom)
			if err != nil {
				return nil, false, err
			}

			if !seenRom {
				err = deduper.Declare(rom)
				if err != nil {
					glog.Errorf("error deduping rom %s: %v", rom.Name, err)
					return nil, false, err
				}

				if fixGame == nil {
					fixGame = new(types.Game)
					fixGame.Name = game.Name
					fixGame.Description = game.Description
				}

				fixGame.Roms = append(fixGame.Roms, rom)
			}
			continue
		}

		foundRom = true

		src, err := gzip.NewReader(romGZ)
		if err != nil {
			glog.Errorf("error opening rom gz file %s: %v", rom.Name, err)
			return nil, false, err
		}

		var dstWriter io.WriteCloser

		if unzipGame {
			romPath := filepath.Join(gamePath, rom.Name)
			if strings.ContainsRune(rom.Name, filepath.Separator) {
				err := os.MkdirAll(filepath.Dir(romPath), 0777)
				if err != nil {
					glog.Errorf("error mkdir %s: %v", filepath.Dir(romPath), err)
					return nil, false, err
				}
			}
			dst, err := os.Create(romPath)
			if err != nil {
				glog.Errorf("error creating destination rom file %s: %v", dst, err)
				return nil, false, err
			}
			dstWriter = dst
		} else {
			dst, err := gameTorrent.Create(rom.Name)
			if err != nil {
				glog.Errorf("error creating torrentzip rom entry %s: %v", rom.Name, err)
				return nil, false, err
			}
			dstWriter = nopWriterCloser{dst}
		}
		_, err = io.Copy(dstWriter, src)
		if err != nil {
			glog.Errorf("error copying rom %s: %v", rom.Name, err)
			return nil, false, err
		}

		src.Close()
		dstWriter.Close()

		romGZ.Close()
	}
	return fixGame, foundRom, nil
}
