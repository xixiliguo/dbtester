// Copyright 2016 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/coreos/dbtester/remotestorage"
	"github.com/coreos/pkg/capnslog"
	"github.com/gyuho/psn/process"
	"github.com/spf13/cobra"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

type Flags struct {
	GRPCPort         string
	WorkingDirectory string
}

// ZookeeperConfig is zookeeper configuration.
// http://zookeeper.apache.org/doc/trunk/zookeeperAdmin.html
type ZookeeperConfig struct {
	TickTime       int
	DataDir        string
	ClientPort     string
	InitLimit      int
	SyncLimit      int
	MaxClientCnxns int64
	SnapCount      int64
	Peers          []ZookeeperPeer
}

// ZookeeperPeer defines Zookeeper peer configuration.
type ZookeeperPeer struct {
	MyID int
	IP   string
}

var (
	shell = os.Getenv("SHELL")

	agentLogPath    = "agent.log"
	databaseLogPath = "database.log"
	monitorLogPath  = "monitor.csv"

	etcdBinaryPath   = filepath.Join(os.Getenv("GOPATH"), "bin/etcd")
	consulBinaryPath = filepath.Join(os.Getenv("GOPATH"), "bin/consul")
	zetcdBinaryPath  = filepath.Join(os.Getenv("GOPATH"), "bin/zetcd")
	cetcdBinaryPath  = filepath.Join(os.Getenv("GOPATH"), "bin/cetcd")

	javaBinaryPath = "/usr/bin/java"

	etcdToken     = "etcd_token"
	etcdDataDir   = "data.etcd"
	consulDataDir = "data.consul"

	zkWorkingDir = "zookeeper"
	zkDataDir    = "zookeeper/data.zk"
	zkConfigPath = "zookeeper.config"
	zkTemplate   = `tickTime={{.TickTime}}
dataDir={{.DataDir}}
clientPort={{.ClientPort}}
initLimit={{.InitLimit}}
syncLimit={{.SyncLimit}}
maxClientCnxns={{.MaxClientCnxns}}
snapCount={{.SnapCount}}
{{range .Peers}}server.{{.MyID}}={{.IP}}:2888:3888
{{end}}
`
	zkConfigDefault = ZookeeperConfig{
		TickTime:       2000,
		ClientPort:     "2181",
		InitLimit:      5,
		SyncLimit:      5,
		MaxClientCnxns: 60,
		Peers: []ZookeeperPeer{
			{MyID: 1, IP: ""},
			{MyID: 2, IP: ""},
			{MyID: 3, IP: ""},
		},
	}
)

var (
	Command = &cobra.Command{
		Use:   "agent",
		Short: "Database agent in remote servers.",
		RunE:  CommandFunc,
	}

	globalFlags = Flags{}
)

func init() {
	if len(shell) == 0 {
		shell = "sh"
	}
	Command.PersistentFlags().StringVar(&globalFlags.GRPCPort, "agent-port", ":3500", "Port to server agent gRPC server.")
	Command.PersistentFlags().StringVar(&globalFlags.WorkingDirectory, "working-directory", homeDir(), "Working directory.")
}

func CommandFunc(cmd *cobra.Command, args []string) error {
	if !exist(globalFlags.WorkingDirectory) {
		return fmt.Errorf("%s does not exist", globalFlags.WorkingDirectory)
	}
	if !filepath.HasPrefix(agentLogPath, globalFlags.WorkingDirectory) {
		agentLogPath = filepath.Join(globalFlags.WorkingDirectory, agentLogPath)
	}

	f, err := openToAppend(agentLogPath)
	if err != nil {
		return err
	}
	defer f.Close()

	capnslog.SetFormatter(capnslog.NewPrettyFormatter(f, false))

	plog.Infof("started serving gRPC %s", globalFlags.GRPCPort)

	var (
		grpcServer = grpc.NewServer()
		sender     = NewTransporterServer()
	)
	ln, err := net.Listen("tcp", globalFlags.GRPCPort)
	if err != nil {
		return err
	}

	RegisterTransporterServer(grpcServer, sender)

	return grpcServer.Serve(ln)
}

type transporterServer struct { // satisfy TransporterServer
	req Request

	cmd     *exec.Cmd
	logfile *os.File
	pid     int

	proxyCmd     *exec.Cmd
	proxyLogfile *os.File
	proxyPid     int
}

var uploadSig = make(chan Request_Operation, 1)

func (t *transporterServer) Transfer(ctx context.Context, r *Request) (*Response, error) {
	peerIPs := strings.Split(r.PeerIPString, "___")
	if r.Operation == Request_Start {
		if !filepath.HasPrefix(etcdDataDir, globalFlags.WorkingDirectory) {
			etcdDataDir = filepath.Join(globalFlags.WorkingDirectory, etcdDataDir)
		}
		if !filepath.HasPrefix(consulDataDir, globalFlags.WorkingDirectory) {
			consulDataDir = filepath.Join(globalFlags.WorkingDirectory, consulDataDir)
		}
		if !filepath.HasPrefix(zkWorkingDir, globalFlags.WorkingDirectory) {
			zkWorkingDir = filepath.Join(globalFlags.WorkingDirectory, zkWorkingDir)
		}
		if !filepath.HasPrefix(zkDataDir, globalFlags.WorkingDirectory) {
			zkDataDir = filepath.Join(globalFlags.WorkingDirectory, zkDataDir)
		}
		if !filepath.HasPrefix(databaseLogPath, globalFlags.WorkingDirectory) {
			databaseLogPath = filepath.Join(globalFlags.WorkingDirectory, databaseLogPath)
		}
		if !filepath.HasPrefix(monitorLogPath, globalFlags.WorkingDirectory) {
			monitorLogPath = filepath.Join(globalFlags.WorkingDirectory, monitorLogPath)
		}

		plog.Info("received gRPC request")
		plog.Infof("working_directory: %q", globalFlags.WorkingDirectory)
		plog.Infof("working_directory_zookeeper: %q", zkWorkingDir)
		plog.Infof("data_directory_etcd: %q", etcdDataDir)
		plog.Infof("data_directory_consul: %q", consulDataDir)
		plog.Infof("data_directory_zookeeper: %q", zkDataDir)
		plog.Infof("database_log_path: %q", databaseLogPath)
		plog.Infof("monitor_log_path: %q", monitorLogPath)
	}

	switch r.Operation {
	case Request_Start:
		t.req = *r
	case Request_UploadLog:
		t.req.GoogleCloudProjectName = r.GoogleCloudProjectName
		t.req.GoogleCloudStorageBucketName = r.GoogleCloudStorageBucketName
		t.req.GoogleCloudStorageSubDirectory = r.GoogleCloudStorageSubDirectory
	}

	if t.req.GoogleCloudStorageKey != "" {
		if err := toFile(t.req.GoogleCloudStorageKey, filepath.Join(globalFlags.WorkingDirectory, "gcloud-key.json")); err != nil {
			return nil, err
		}
	}

	var pidToMonitor int
	switch r.Operation {
	case Request_Start:
		switch t.req.Database {
		case Request_etcdv2, Request_etcdv3, Request_zetcd, Request_cetcd:
			if !exist(etcdBinaryPath) {
				return nil, fmt.Errorf("etcd binary %q does not exist", etcdBinaryPath)
			}
			if t.req.Database == Request_zetcd && !exist(zetcdBinaryPath) {
				return nil, fmt.Errorf("zetcd binary %q does not exist", zetcdBinaryPath)
			}
			if t.req.Database == Request_cetcd && !exist(cetcdBinaryPath) {
				return nil, fmt.Errorf("cetcd binary %q does not exist", cetcdBinaryPath)
			}
			if err := os.RemoveAll(etcdDataDir); err != nil {
				return nil, err
			}
			f, err := openToAppend(databaseLogPath)
			if err != nil {
				return nil, err
			}
			t.logfile = f

			clusterN := len(peerIPs)
			names := make([]string, clusterN)
			clientURLs := make([]string, clusterN)
			peerURLs := make([]string, clusterN)
			members := make([]string, clusterN)
			for i, u := range peerIPs {
				names[i] = fmt.Sprintf("etcd-%d", i+1)
				clientURLs[i] = fmt.Sprintf("http://%s:2379", u)
				peerURLs[i] = fmt.Sprintf("http://%s:2380", u)
				members[i] = fmt.Sprintf("%s=%s", names[i], peerURLs[i])
			}
			clusterStr := strings.Join(members, ",")
			flags := []string{
				"--name", names[t.req.ServerIndex],
				"--data-dir", etcdDataDir,

				"--listen-client-urls", clientURLs[t.req.ServerIndex],
				"--advertise-client-urls", clientURLs[t.req.ServerIndex],

				"--listen-peer-urls", peerURLs[t.req.ServerIndex],
				"--initial-advertise-peer-urls", peerURLs[t.req.ServerIndex],

				"--initial-cluster-token", etcdToken,
				"--initial-cluster", clusterStr,
				"--initial-cluster-state", "new",
			}
			flagString := strings.Join(flags, " ")

			cmd := exec.Command(etcdBinaryPath, flags...)
			cmd.Stdout = f
			cmd.Stderr = f

			cmdString := fmt.Sprintf("%s %s", cmd.Path, flagString)
			plog.Infof("starting binary %q", cmdString)
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			t.cmd = cmd
			t.pid = cmd.Process.Pid
			plog.Infof("started binary %q [PID: %d]", cmdString, t.pid)
			pidToMonitor = t.pid
			go func() {
				if err := cmd.Wait(); err != nil {
					plog.Errorf("cmd.Wait %q returned error %v", cmdString, err)
					return
				}
				plog.Infof("exiting %q", cmdString)
			}()

			if t.req.Database == Request_zetcd || t.req.Database == Request_cetcd {
				f2, err := openToAppend(databaseLogPath + "-" + t.req.Database.String())
				if err != nil {
					return nil, err
				}
				t.proxyLogfile = f2
				var flags2 []string
				if t.req.Database == Request_zetcd {
					flags2 = []string{
						"-zkaddr", "0.0.0.0:2181",
						"-endpoint", clientURLs[t.req.ServerIndex], // etcd endpoint
					}
				} else {
					flags2 = []string{
						"-consuladdr", "0.0.0.0:8500",
						"-etcd", clientURLs[t.req.ServerIndex], // etcd endpoint
					}
				}
				flagString2 := strings.Join(flags2, " ")

				bpath := zetcdBinaryPath
				if t.req.Database == Request_cetcd {
					bpath = cetcdBinaryPath
				}
				cmd2 := exec.Command(bpath, flags2...)
				cmd2.Stdout = f2
				cmd2.Stderr = f2

				cmdString2 := fmt.Sprintf("%s %s", cmd2.Path, flagString2)
				plog.Infof("starting binary %q", cmdString2)
				if err := cmd2.Start(); err != nil {
					return nil, err
				}
				t.proxyCmd = cmd2
				t.proxyPid = cmd2.Process.Pid
				plog.Infof("started binary %q [PID: %d]", cmdString2, t.proxyPid)
				go func() {
					if err := cmd2.Wait(); err != nil {
						plog.Errorf("cmd.Wait %q returned error %v", cmdString2, err)
						return
					}
					plog.Infof("exiting %q", cmdString2)
				}()
			}

		case Request_ZooKeeper:
			if !exist(javaBinaryPath) {
				return nil, fmt.Errorf("%q does not exist", javaBinaryPath)
			}

			plog.Infof("os.Chdir %q", zkWorkingDir)
			if err := os.Chdir(zkWorkingDir); err != nil {
				return nil, err
			}

			plog.Infof("os.MkdirAll %q", zkDataDir)
			if err := os.MkdirAll(zkDataDir, 0777); err != nil {
				return nil, err
			}

			idFilePath := filepath.Join(zkDataDir, "myid")
			plog.Infof("writing zk myid file %d in %s", t.req.ZookeeperMyID, idFilePath)
			if err := toFile(fmt.Sprintf("%d", t.req.ZookeeperMyID), idFilePath); err != nil {
				return nil, err
			}

			// generate zookeeper config
			zkCfg := zkConfigDefault
			zkCfg.DataDir = zkDataDir
			peers := []ZookeeperPeer{}
			for i := range peerIPs {
				peers = append(peers, ZookeeperPeer{MyID: i + 1, IP: peerIPs[i]})
			}
			zkCfg.Peers = peers
			zkCfg.MaxClientCnxns = t.req.ZookeeperMaxClientCnxns
			zkCfg.SnapCount = t.req.ZookeeperSnapCount
			tpl := template.Must(template.New("zkTemplate").Parse(zkTemplate))
			buf := new(bytes.Buffer)
			if err := tpl.Execute(buf, zkCfg); err != nil {
				return nil, err
			}
			zc := buf.String()

			configFilePath := filepath.Join(zkWorkingDir, zkConfigPath)
			plog.Infof("writing zk config file %q (config %q)", configFilePath, zc)
			if err := toFile(zc, configFilePath); err != nil {
				return nil, err
			}

			f, err := openToAppend(databaseLogPath)
			if err != nil {
				return nil, err
			}
			t.logfile = f

			// TODO: change for different releases
			// https://zookeeper.apache.org/doc/trunk/zookeeperAdmin.html
			flagString := `-cp zookeeper-3.4.9.jar:lib/slf4j-api-1.6.1.jar:lib/slf4j-log4j12-1.6.1.jar:lib/log4j-1.2.16.jar:conf org.apache.zookeeper.server.quorum.QuorumPeerMain`
			args := []string{shell, "-c", javaBinaryPath + " " + flagString + " " + configFilePath}

			cmd := exec.Command(args[0], args[1:]...)
			cmd.Stdout = f
			cmd.Stderr = f

			cmdString := fmt.Sprintf("%s %s", cmd.Path, strings.Join(args[1:], " "))
			plog.Infof("starting binary %q", cmdString)
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			t.cmd = cmd
			t.pid = cmd.Process.Pid
			plog.Infof("started binary %q [PID: %d]", cmdString, t.pid)
			pidToMonitor = t.pid
			go func() {
				if err := cmd.Wait(); err != nil {
					plog.Error("cmd.Wait returned error", cmdString, err)
					return
				}
				plog.Infof("exiting %q (%v)", cmdString, err)
			}()

		case Request_Consul:
			if !exist(consulBinaryPath) {
				return nil, fmt.Errorf("%q does not exist", consulBinaryPath)
			}
			if err := os.RemoveAll(consulDataDir); err != nil {
				return nil, err
			}
			f, err := openToAppend(databaseLogPath)
			if err != nil {
				return nil, err
			}
			t.logfile = f

			var flags []string
			switch t.req.ServerIndex {
			case 0: // leader
				flags = []string{
					"agent",
					"-server",
					"-data-dir", consulDataDir,
					"-bind", peerIPs[t.req.ServerIndex],
					"-client", peerIPs[t.req.ServerIndex],
					"-bootstrap-expect", "3",
				}

			default:
				flags = []string{
					"agent",
					"-server",
					"-data-dir", consulDataDir,
					"-bind", peerIPs[t.req.ServerIndex],
					"-client", peerIPs[t.req.ServerIndex],
					"-join", peerIPs[0],
				}
			}
			flagString := strings.Join(flags, " ")

			cmd := exec.Command(consulBinaryPath, flags...)
			cmd.Stdout = f
			cmd.Stderr = f

			cmdString := fmt.Sprintf("%s %s", cmd.Path, flagString)
			plog.Infof("starting binary %q", cmdString)
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			t.cmd = cmd
			t.pid = cmd.Process.Pid
			plog.Infof("started binary %q [PID: %d]", cmdString, t.pid)
			pidToMonitor = t.pid
			go func() {
				if err := cmd.Wait(); err != nil {
					plog.Error("cmd.Wait returned error", cmdString, err)
					return
				}
				plog.Infof("exiting %q (%v)", cmdString, err)
			}()

		default:
			return nil, fmt.Errorf("unknown database %q", r.Database)
		}

	case Request_Stop:
		time.Sleep(3 * time.Second) // wait a few more seconds to collect more monitoring data
		if t.cmd == nil {
			return nil, fmt.Errorf("nil command")
		}
		plog.Infof("stopping binary %q for %q [PID: %d]", t.cmd.Path, t.req.Database.String(), t.pid)
		if err := syscall.Kill(t.pid, syscall.SIGTERM); err != nil {
			return nil, err
		}
		if t.logfile != nil {
			t.logfile.Close()
		}
		plog.Infof("stopped binary %q [PID: %d]", t.req.Database.String(), t.pid)
		pidToMonitor = t.pid

		if t.proxyCmd != nil {
			plog.Infof("stopping proxy binary %q for %q [PID: %d]", t.proxyCmd.Path, t.req.Database.String(), t.proxyPid)
			if err := syscall.Kill(t.proxyPid, syscall.SIGTERM); err != nil {
				return nil, err
			}
			plog.Infof("stopped proxy binary %q for %q [PID: %d]", t.proxyCmd.Path, t.req.Database.String(), t.proxyPid)
		}
		if t.proxyLogfile != nil {
			t.proxyLogfile.Close()
		}
		uploadSig <- Request_Stop

	case Request_UploadLog:
		time.Sleep(3 * time.Second) // wait a few more seconds to collect more monitoring data
		plog.Infof("just uploading logs without stoppping %q [PID: %d]", t.req.Database.String(), t.pid)
		if t.logfile != nil {
			t.logfile.Sync()
		}
		uploadSig <- Request_UploadLog

	default:
		return nil, fmt.Errorf("Not implemented %v", r.Operation)
	}

	if r.Operation == Request_Start {
		go func(pid int) {
			notifier := make(chan os.Signal, 1)
			signal.Notify(notifier, syscall.SIGINT, syscall.SIGTERM)

			rFunc := func() error {
				pss, err := process.List(&process.Process{Stat: process.Stat{Pid: int64(pid)}})
				if err != nil {
					return err
				}

				f, err := openToAppend(monitorLogPath)
				if err != nil {
					return err
				}
				defer f.Close()

				return process.WriteToCSV(f, pss...)
			}

			plog.Infof("saving monitoring results for %q in %q", t.req.Database.String(), monitorLogPath)
			var err error
			if err = rFunc(); err != nil {
				plog.Errorf("monitoring error (%v)", err)
				return
			}

			uploadFunc := func() error {
				plog.Infof("stopped monitoring, uploading to storage %q", t.req.GoogleCloudProjectName)
				u, err := remotestorage.NewGoogleCloudStorage([]byte(t.req.GoogleCloudStorageKey), t.req.GoogleCloudProjectName)
				if err != nil {
					return err
				}

				srcDatabaseLogPath := databaseLogPath
				dstDatabaseLogPath := filepath.Base(databaseLogPath)
				if !strings.HasPrefix(filepath.Base(databaseLogPath), t.req.TestName) {
					dstDatabaseLogPath = fmt.Sprintf("%s-%d-%s", t.req.TestName, t.req.ServerIndex+1, filepath.Base(databaseLogPath))
				}
				dstDatabaseLogPath = filepath.Join(t.req.GoogleCloudStorageSubDirectory, dstDatabaseLogPath)
				plog.Infof("uploading database log [%q -> %q]", srcDatabaseLogPath, dstDatabaseLogPath)
				var uerr error
				for k := 0; k < 30; k++ {
					if uerr = u.UploadFile(t.req.GoogleCloudStorageBucketName, srcDatabaseLogPath, dstDatabaseLogPath); uerr != nil {
						plog.Errorf("u.UploadFile error... sleep and retry... (%v)", uerr)
						time.Sleep(2 * time.Second)
						continue
					} else {
						break
					}
				}

				if t.req.Database == Request_zetcd || t.req.Database == Request_cetcd {
					dpath := databaseLogPath + "-" + t.req.Database.String()
					if exist(dpath) {
						srcDatabaseLogPath2 := dpath
						dstDatabaseLogPath2 := filepath.Base(dpath)
						if !strings.HasPrefix(filepath.Base(dpath), t.req.TestName) {
							dstDatabaseLogPath2 = fmt.Sprintf("%s-%d-%s", t.req.TestName, t.req.ServerIndex+1, filepath.Base(dpath))
						}
						dstDatabaseLogPath2 = filepath.Join(t.req.GoogleCloudStorageSubDirectory, dstDatabaseLogPath2)
						plog.Infof("uploading database log [%q -> %q]", srcDatabaseLogPath2, dstDatabaseLogPath2)
						var uerr error
						for k := 0; k < 30; k++ {
							if uerr = u.UploadFile(t.req.GoogleCloudStorageBucketName, srcDatabaseLogPath2, dstDatabaseLogPath2); uerr != nil {
								plog.Errorf("u.UploadFile error... sleep and retry... (%v)", uerr)
								time.Sleep(2 * time.Second)
								continue
							} else {
								break
							}
						}
					} else {
						plog.Errorf("%q is expected, but doesn't exist!", dpath)
					}
				}

				srcMonitorResultPath := monitorLogPath
				dstMonitorResultPath := filepath.Base(monitorLogPath)
				if !strings.HasPrefix(filepath.Base(monitorLogPath), t.req.TestName) {
					dstMonitorResultPath = fmt.Sprintf("%s-%d-%s", t.req.TestName, t.req.ServerIndex+1, filepath.Base(monitorLogPath))
				}
				dstMonitorResultPath = filepath.Join(t.req.GoogleCloudStorageSubDirectory, dstMonitorResultPath)
				plog.Infof("uploading monitor results [%q -> %q]", srcMonitorResultPath, dstMonitorResultPath)
				for k := 0; k < 30; k++ {
					if uerr = u.UploadFile(t.req.GoogleCloudStorageBucketName, srcMonitorResultPath, dstMonitorResultPath); uerr != nil {
						plog.Errorf("u.UploadFile error... sleep and retry... (%v)", uerr)
						time.Sleep(2 * time.Second)
						continue
					} else {
						break
					}
				}

				srcAgentLogPath := agentLogPath
				dstAgentLogPath := filepath.Base(agentLogPath)
				if !strings.HasPrefix(filepath.Base(agentLogPath), t.req.TestName) {
					dstAgentLogPath = fmt.Sprintf("%s-%d-%s", t.req.TestName, t.req.ServerIndex+1, filepath.Base(agentLogPath))
				}
				dstAgentLogPath = filepath.Join(t.req.GoogleCloudStorageSubDirectory, dstAgentLogPath)
				plog.Infof("uploading agent logs [%q -> %q]", srcAgentLogPath, dstAgentLogPath)
				for k := 0; k < 30; k++ {
					if uerr = u.UploadFile(t.req.GoogleCloudStorageBucketName, srcAgentLogPath, dstAgentLogPath); uerr != nil {
						plog.Errorf("u.UploadFile error... sleep and retry... (%v)", uerr)
						time.Sleep(2 * time.Second)
						continue
					} else {
						break
					}
				}

				return nil
			}

			for {
				select {
				case <-time.After(time.Second):
					if err = rFunc(); err != nil {
						plog.Errorf("monitoring error (%v)", err)
						continue
					}

				case us := <-uploadSig:
					if err = uploadFunc(); err != nil {
						plog.Errorf("uploadFunc error (%v)", err)
						return
					}
					if us == Request_UploadLog {
						continue
					}
					return

				case sig := <-notifier:
					plog.Infof("signal received %q", sig.String())
					return
				}
			}
		}(pidToMonitor)
	}

	plog.Info("transfer success")
	return &Response{Success: true}, nil
}

func NewTransporterServer() TransporterServer {
	return &transporterServer{}
}
