/*
 * Copyright(c) 2022 Intel Corporation.
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"flag"
	"fmt"
	"github.com/intel/afxdp-plugins-for-kubernetes/constants"
	"github.com/intel/afxdp-plugins-for-kubernetes/internal/deviceplugin"
	"github.com/intel/afxdp-plugins-for-kubernetes/internal/host"
	"github.com/intel/afxdp-plugins-for-kubernetes/internal/logformats"
	"github.com/intel/afxdp-plugins-for-kubernetes/internal/networking"
	"github.com/intel/afxdp-plugins-for-kubernetes/internal/tools"
	logging "github.com/sirupsen/logrus"
	"io"
	"os"
	"os/signal"
	"syscall"
)

var (
	hostHandler = host.NewHandler()
	netHandler  = networking.NewHandler()
	deviceFile  = constants.DeviceFile.Directory + constants.DeviceFile.Name
)

type devicePlugin struct {
	pools map[string]deviceplugin.PoolManager
}

func main() {
	var configFile string
	flag.StringVar(&configFile, "config", constants.Plugins.DevicePlugin.DefaultConfigFile, "Location of the device plugin configuration file")
	flag.Parse()
	logging.SetReportCaller(true)
	logging.SetFormatter(logformats.Default)

	// overall config
	cfg, err := deviceplugin.GetPluginConfig(configFile)
	if err != nil {
		logging.Errorf("Error getting device plugin config: %v", err)
		exit(constants.Plugins.DevicePlugin.ExitConfigError)
	}

	// logging
	if err := configureLogging(cfg); err != nil {
		logging.Errorf("Error configuring logging: %v", err)
		exit(constants.Plugins.DevicePlugin.ExitLogError)
	}

	//device file
	exists, err := tools.FilePathExists(deviceFile)
	if err != nil {
		logging.Errorf("Error checking device file path: %v", err)
	}
	if exists {
		if err = os.Remove(deviceFile); err != nil {
			logging.Errorf("Error deleting device file: %v", err)
		}
	}

	logging.Infof("Starting AF_XDP Device Plugin")

	// host requirements
	logging.Infof("Checking if host meets requriements")
	hostMeetsRequirements, err := checkHost(hostHandler)
	if err != nil {
		logging.Errorf("Error checking host: %v", err)
		exit(constants.Plugins.DevicePlugin.ExitHostError)
	}
	if !hostMeetsRequirements {
		logging.Infof("Host does not meet requriements")
		exit(constants.Plugins.DevicePlugin.ExitNormal)
	}
	logging.Infof("Host meets requriements")

	// pool configs
	logging.Infof("Getting device pools")
	poolConfigs, err := deviceplugin.GetPoolConfigs(configFile, netHandler, hostHandler)
	if err != nil {
		logging.Warningf("Error getting device pools: %v", err)
		exit(constants.Plugins.DevicePlugin.ExitPoolError)
	}

	dp := devicePlugin{
		pools: make(map[string]deviceplugin.PoolManager),
	}

	for _, poolConfig := range poolConfigs {
		poolManager := deviceplugin.NewPoolManager(poolConfig)

		if err := poolManager.Init(poolConfig); err != nil {
			logging.Errorf("Error initializing pool %v: %v", poolManager.Name, err)
			continue
		}
		dp.pools[poolConfig.Name] = poolManager
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	s := <-sigs
	logging.Infof("Received signal \"%v\"", s)
	for _, pm := range dp.pools {
		logging.Infof("Terminating %v", pm.Name)
		if err := pm.Terminate(); err != nil {
			logging.Errorf("Termination error: %v", err)
		}
	}

}

func configureLogging(cfg deviceplugin.PluginConfig) error {
	var (
		logDir      = constants.Logging.Directory
		logDirPerm  = os.FileMode(constants.Logging.DirectoryPermissions)
		logFile     = cfg.LogFile
		logFilePerm = os.FileMode(constants.Logging.FilePermissions)
		logLevel    = cfg.LogLevel
	)

	if logFile != "" {
		logging.Infof("Setting log directory: %s", logDir)
		err := os.MkdirAll(logDir, logDirPerm)
		if err != nil {
			logging.Errorf("Error setting log directory: %v", err)
			return err
		}

		logging.Infof("Setting log file: %s", logFile)
		fp, err := os.OpenFile(logDir+logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, logFilePerm)
		if err != nil {
			logging.Errorf("Error setting log file: %v", err)
			return err
		}
		logging.SetOutput(io.MultiWriter(fp, os.Stdout))
	}

	if logLevel != "" {
		logging.Infof("Setting log level: %s", logLevel)
		level, err := logging.ParseLevel(logLevel)
		if err != nil {
			logging.Errorf("Error setting log level: %v", err)
			return err
		}
		logging.SetLevel(level)

		if logLevel == "debug" {
			logging.Infof("Switching to debug log format")
			logging.SetFormatter(logformats.Debug)
		}
	}

	return nil
}

func checkHost(host host.Handler) (bool, error) {
	// kernel
	logging.Debugf("Checking kernel version")
	linuxVersion, err := host.KernelVersion()
	if err != nil {
		err := fmt.Errorf("Error checking kernel version: %v", err)
		return false, err

	}

	linuxInt, err := tools.KernelVersionInt(linuxVersion)
	if err != nil {
		err := fmt.Errorf("Error converting actual kernel version to int: %v", err)
		return false, err

	}

	minLinuxInt, err := tools.KernelVersionInt(constants.Afxdp.MinumumKernel)
	if err != nil {
		err := fmt.Errorf("Error converting minimum kernel version to int: %v", err)
		return false, err

	}

	if linuxInt < minLinuxInt {
		logging.Warningf("Kernel version %v is below minimum requirement %v", linuxVersion, constants.Afxdp.MinumumKernel)
		return false, nil
	}
	logging.Debugf("Kernel version: %v meets minimum requirements", linuxVersion)

	// libbpf
	logging.Debugf("Checking host for Libbpf")
	bpfInstalled, libs, err := host.HasLibbpf()
	if err != nil {
		err := fmt.Errorf("Libbpf not found on host")
		return false, err
	}
	if bpfInstalled {
		logging.Debugf("Libbpf found on host:")
		for _, lib := range libs {
			logging.Debugf("\t" + lib)
		}
	} else {
		logging.Warningf("Libbpf not found on host")
		return false, nil
	}

	return true, nil
}

func exit(code int) {
	if code == 0 {
		logging.Infof("Device plugin will exit")
	} else {
		logging.Errorf("Device plugin will exit")
	}
	os.Exit(code)
}
