/*
 * Copyright (c) 2022-2023 NVIDIA CORPORATION.  All rights reserved.
 *
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


/*
 * main()
  └─> newApp()                          // 创建 CLI 应用
      └─> app.Run(os.Args)              // 启动 CLI
          └─> app.RunContext(ctx, args) // urfave/cli 内部
              └─> rootCommand.Run(cCtx, args) // urfave/cli 内部
                  ├─> parseFlags()       // 解析标志 → 写入 Destination
                  ├─> Before(cCtx)       // main.go:279
                  │   └─> loggingConfig.Apply()
                  ├─> Action(cCtx)       // main.go:297 ← 核心！
                  │   └─> RunPlugin(ctx, config) // main.go:352
                  │       └─> NewDriver() → 阻塞运行 → Shutdown()
                  └─> After(cCtx) (defer) // main.go:320
                      └─> logs.FlushLogs()
 */


package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	// urfave/cli是一个CLI框架，用于解析命令行参数和环境变量
	// 提供统一的命令行接口，支持标志、子命令、帮助文本等
	"github.com/urfave/cli/v2"

	// k8s.io/component-base/logs提供Kubernetes组件日志库
	// 提供日志刷新功能确保程序退出前日志写入完成
	"k8s.io/component-base/logs"
	
	// k8s.io/dynamic-resource-allocation/kubeletplugin是K8s DRA框架的kubelet插件库
	// 提供标准的插件注册路径常量和注册机制
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	
	// k8s.io/klog是Kubernetes结构化日志库
	// 用于记录插件运行时的各种事件和错误，支持日志级别控制
	"k8s.io/klog/v2"

	// 项目通用工具包，提供调试信号处理等功能
	"github.com/NVIDIA/k8s-dra-driver-gpu/internal/common"
	
	// 项目版本信息包，用于显示插件版本
	"github.com/NVIDIA/k8s-dra-driver-gpu/internal/info"
	
	// 项目的标志位配置包，提供日志、特性门控和Kubernetes客户端配置
	pkgflags "github.com/NVIDIA/k8s-dra-driver-gpu/pkg/flags"
)

const (
	// DriverName 是DRA驱动的唯一标识符
	// 必须与ResourceClass中的driverName字段匹配，确保全局唯一性，避免与其他驱动冲突
	DriverName = "gpu.nvidia.com"
	
	// DriverPluginCheckpointFileBasename 是检查点文件的基础名称
	// 检查点文件用于在插件重启时恢复状态，存储已分配的资源信息
	// 这对于保证资源分配的持久性和一致性至关重要，防止重启后资源泄漏
	DriverPluginCheckpointFileBasename = "checkpoint.json"
)

// Flags 结构体定义插件运行所需的所有配置参数
type Flags struct {
	// kubeClientConfig 包含连接Kubernetes API服务器所需的配置
	// 包括kubeconfig路径、QPS限制等，这是与K8s集群通信的基础，用于监听ResourceClaim等资源的变化
	kubeClientConfig pkgflags.KubeClientConfig

	// nodeName 是当前节点的名称，通过环境变量NODE_NAME从downward API获取
	// DRA插件需要知道自己运行在哪个节点上，以便只处理调度到本节点的Pod的资源请求
	// 这是必需参数，因为插件必须明确知道要管理哪个节点的GPU资源
	nodeName string
	
	// namespace 是存储自定义资源（如DeviceState、NodeState）的命名空间
	// 默认为"default"，这些CR用于持久化GPU的状态信息和资源分配记录
	namespace string
	
	// cdiRoot 是CDI（Container Device Interface）规范文件的生成目录，用于描述如何将设备暴露给容器
	// 默认为/etc/cdi，这是CDI规范推荐的标准位置，容器运行时会自动扫描此目录
	cdiRoot string
	
	// containerDriverRoot 是NVIDIA驱动在容器内的挂载路径
	// 用于在生成CDI规范时指定驱动文件的路径，容器运行时会根据CDI规范挂载这些文件到容器中
	containerDriverRoot string
	
	// hostDriverRoot 是NVIDIA驱动在宿主机上的安装根路径
	// 通常是"/"或"/run/nvidia/driver"，插件需要访问驱动文件来生成CDI规范
	hostDriverRoot string
	
	// nvidiaCDIHookPath 是nvidia-cdi-hook可执行文件在宿主机文件系统中的绝对路径
	// 这个hook在容器启动时执行，用于设置GPU设备的访问权限和环境变量
	// 必须是宿主机路径，因为容器运行时在宿主机上执行这个hook
	nvidiaCDIHookPath string
	
	// imageName 是当前插件容器镜像的完整名称（包含registry、tag等）
	// 用于渲染各种配置模板，确保组件版本一致性
	imageName string
	
	// kubeletRegistrarDirectoryPath 是kubelet存储插件注册信息的目录
	// 默认为/var/lib/kubelet/plugins_registry
	// kubelet的plugin watcher监控此目录，插件在此创建socket文件来注册自己
	kubeletRegistrarDirectoryPath string
	
	// kubeletPluginsDirectoryPath 是kubelet存储插件数据的目录
	// 默认为/var/lib/kubelet/plugins
	// 插件在此目录下创建自己的子目录存储状态数据
	kubeletPluginsDirectoryPath string
	
	// healthcheckPort 是gRPC健康检查服务的端口号
	// 正数表示固定端口，0表示随机分配，负数表示禁用健康检查
	// 默认为-1（禁用），生产环境建议启用以便监控插件健康状态
	healthcheckPort int
	
	// additionalXidsToIgnore 是要忽略的额外XID错误代码列表（逗号分隔）
	// XID是NVIDIA GPU的硬件错误代码
	// 某些非致命错误可以忽略避免误报，提供运维灵活性
	additionalXidsToIgnore string
}

// Config 结构体封装插件运行时的完整配置
// 包含命令行标志和Kubernetes客户端集合，作为插件的核心配置对象
type Config struct {
	// flags 是从命令行或环境变量解析出的配置参数
	flags *Flags
	
	// clientsets 是与Kubernetes API交互的客户端集合
	// 包括标准的kubernetes clientset和自定义资源的clientset
	// 用于读取Pod信息、更新ResourceClaim状态、操作自定义资源等
	clientsets pkgflags.ClientSets
}

// DriverPluginPath 返回DRA插件的工作目录路径
// 这个目录用于存储插件的socket文件、检查点文件、nvidia-cdi-hook等
// 路径格式为：{kubeletPluginsDirectoryPath}/{DriverName}
// 例如：/var/lib/kubelet/plugins/gpu.nvidia.com
func (c Config) DriverPluginPath() string {
	return filepath.Join(c.flags.kubeletPluginsDirectoryPath, DriverName)
}

func main() {
	// 调用newApp()创建CLI应用实例，然后用os.Args执行它
	// os.Args包含所有命令行参数（第一个元素是程序名）
	if err := newApp().Run(os.Args); err != nil {
		// 如果运行过程中发生错误，格式化输出到标准错误流
		// 使用Fprintf而不是直接panic，是为了提供更友好的错误信息
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		// 以非零状态码退出，告知操作系统或容器编排器程序异常终止
		// 这对于Kubernetes等编排系统判断容器健康状态很重要
		os.Exit(1)
	}
}

// newApp 创建并配置CLI应用，定义所有命令行标志、执行逻辑和生命周期钩子
func newApp() *cli.App {
	// 创建日志配置对象，用于控制日志级别、格式等
	// 日志配置必须在任何日志输出之前应用，否则配置不会生效
	loggingConfig := pkgflags.NewLoggingConfig()
	
	// 创建特性门控配置对象，用于控制实验性功能的开关
	// 这允许在不修改代码的情况下启用或禁用某些功能，便于渐进式发布新特性
	featureGateConfig := pkgflags.NewFeatureGateConfig()
	
	// 创建标志位配置对象，用于存储所有命令行参数的值
	flags := &Flags{}

	// 定义所有命令行标志的配置，CLI 框架解析时自动按优先级选择：命令行 > 环境变量 > 默认值
	// 配置定义与配置使用分离，通过 Destination 指针实现自动赋值
	cliFlags := []cli.Flag{
		&cli.StringFlag{
			Name:     "node-name",
			Usage:    "The name of the node to be worked on.",
			Required: true, // 必需参数，因为插件必须知道运行在哪个节点
			Destination: &flags.nodeName,
			EnvVars:  []string{"NODE_NAME"}, // 可通过NODE_NAME环境变量设置，通常从K8s downward API获取
		},
		&cli.StringFlag{
			Name:  "namespace",
			Usage: "The namespace used for the custom resources.",
			Value: "default", // 默认使用default命名空间
			Destination: &flags.namespace,
			EnvVars: []string{"NAMESPACE"},
		},
		&cli.StringFlag{
			Name:  "cdi-root",
			Usage: "Absolute path to the directory where CDI files will be generated.",
			Value: "/etc/cdi", // 使用CDI标准推荐的路径
			Destination: &flags.cdiRoot,
			EnvVars: []string{"CDI_ROOT"},
		},
		&cli.StringFlag{
			Name:    "nvidia-driver-root",
			Aliases: []string{"host_driver-root"}, // 提供别名以保持向后兼容
			Value:   "/",
			Usage:   "the root path for the NVIDIA driver installation on the host (typical values are '/' or '/run/nvidia/driver')",
			Destination: &flags.hostDriverRoot,
			EnvVars: []string{"NVIDIA_DRIVER_ROOT", "HOST_DRIVER_ROOT"},
		},
		&cli.StringFlag{
			Name:  "container-driver-root",
			Value: "/driver-root",
			Usage: "the path where the NVIDIA driver root is mounted in the container; used for generating CDI specifications",
			Destination: &flags.containerDriverRoot,
			EnvVars: []string{"DRIVER_ROOT_CTR_PATH"},
		},
		&cli.StringFlag{
			Name:  "nvidia-cdi-hook-path",
			Usage: "Absolute path to the nvidia-cdi-hook executable in the host file system. Used in the generated CDI specification.",
			// 注意这里没有默认值，如果未设置，稍后会通过setNvidiaCDIHookPath()方法自动配置
			Destination: &flags.nvidiaCDIHookPath,
			EnvVars: []string{"NVIDIA_CDI_HOOK_PATH"},
		},
		&cli.StringFlag{
			Name:     "image-name",
			Usage:    "The full image name to use for rendering templates.",
			Required: true, // 必需参数，确保版本一致性
			Destination: &flags.imageName,
			EnvVars: []string{"IMAGE_NAME"},
		},
		&cli.StringFlag{
			Name:  "kubelet-registrar-directory-path",
			Usage: "Absolute path to the directory where kubelet stores plugin registrations.",
			Value: kubeletplugin.KubeletRegistryDir, // 使用K8s DRA框架提供的标准路径常量
			Destination: &flags.kubeletRegistrarDirectoryPath,
			EnvVars: []string{"KUBELET_REGISTRAR_DIRECTORY_PATH"},
		},
		&cli.StringFlag{
			Name:  "kubelet-plugins-directory-path",
			Usage: "Absolute path to the directory where kubelet stores plugin data.",
			Value: kubeletplugin.KubeletPluginsDir, // 使用K8s DRA框架提供的标准路径常量
			Destination: &flags.kubeletPluginsDirectoryPath,
			EnvVars: []string{"KUBELET_PLUGINS_DIRECTORY_PATH"},
		},
		&cli.IntFlag{
			Name:  "healthcheck-port",
			Usage: "Port to start a gRPC healthcheck service. When positive, a literal port number. When zero, a random port is allocated. When negative, the healthcheck service is disabled.",
			Value: -1, // 默认禁用健康检查
			Destination: &flags.healthcheckPort,
			EnvVars: []string{"HEALTHCHECK_PORT"},
		},
		// TODO: change to StringSliceFlag.
		// 未来应该改为StringSliceFlag以更好地处理列表
		// 当前使用逗号分隔的字符串作为临时方案
		&cli.StringFlag{
			Name:  "additional-xids-to-ignore",
			Usage: "A comma-separated list of additional XIDs to ignore.",
			Value: "",
			Destination: &flags.additionalXidsToIgnore,
			EnvVars: []string{"ADDITIONAL_XIDS_TO_IGNORE"},
		},
	}
	
	// 追加Kubernetes客户端配置的标志（如kubeconfig路径、QPS限制等）
	// 这些标志定义在kubeClientConfig中，用于配置与K8s API的连接
	cliFlags = append(cliFlags, flags.kubeClientConfig.Flags()...)
	
	// 追加特性门控配置的标志
	// 允许通过命令行启用或禁用实验性特性，便于渐进式发布
	cliFlags = append(cliFlags, featureGateConfig.Flags()...)
	
	// 追加日志配置的标志（如日志级别、日志格式等）
	// 这些是klog的标准配置选项
	cliFlags = append(cliFlags, loggingConfig.Flags()...)

	// 创建CLI应用实
	app := &cli.App{
		Name:            "gpu-kubelet-plugin",
		Usage:           "gpu-kubelet-plugin implements a DRA driver plugin for NVIDIA GPUs.",
		ArgsUsage:       " ", // 将 myapp [arguments...] 变为 myapp，不接受 arg1 arg2 位置参数，只接受 --node-name=worker1 标志参数
		HideHelpCommand: true, // 隐藏默认的help子命令，因为这是一个简单的单命令应用
		Flags:           cliFlags,

		// Before钩子在Action之前执行，用于参数验证和初始化
		Before: func(c *cli.Context) error {
			// 检查是否有意外的位置参数，DRA插件不接受位置参数，所有配置都应通过标志或环境变量提供
			if c.Args().Len() > 0 {
				return fmt.Errorf("arguments not supported: %v", c.Args().Slice())
			}
			// loggingConfig must be applied before doing any logging
			// 应用日志配置，这必须在任何日志输出之前完成，否则日志级别等配置不会生效
			err := loggingConfig.Apply()
			
			// 记录启动配置信息，便于调试和故障排查，会输出所有重要的配置参数值
			pkgflags.LogStartupConfig(flags, loggingConfig)
			return err
		},
		
		// Action是主要的业务逻辑入口
		Action: func(c *cli.Context) error {
			// 根据kubeClientConfig创建Kubernetes客户端集合
			// 包括标准clientset和自定义资源clientset
			clientSets, err := flags.kubeClientConfig.NewClientSets()
			if err != nil {
				return fmt.Errorf("create client: %w", err)
			}

			// 创建插件运行时配置对象，封装flags和clientsets
			// 这个config对象会传递给RunPlugin，包含运行插件所需的所有信息
			config := &Config{
				flags:      flags,
				clientsets: clientSets,
			}

			// 运行插件的主逻辑
			// c.Context包含应用级别的context，用于控制插件生命周期
			return RunPlugin(c.Context, config)
		},
		
		// After钩子在Action之后执行（无论成功或失败）
		// 用于清理资源和确保日志写入完成
		After: func(c *cli.Context) error {
			// Runs after `Action` (regardless of success/error). In urfave cli
			// v2, the final error reported will be from either Action, Before,
			// or After (whichever is non-nil and last executed).
			// 记录关闭日志
			klog.Infof("shutdown")
			
			// 刷新所有日志缓冲区，确保日志完全写入到目标（文件或stderr）
			// 这对于不丢失最后的日志消息很重要，特别是在容器环境中
			logs.FlushLogs()
			return nil
		},
		
		// 设置版本信息，用于--version标志
		Version: info.GetVersionString(),
	}

	// We remove the -v alias for the version flag so as to not conflict with the -v flag used for klog.
	// 移除 -v 版本标志别名，避免与klog的 -v 日志级别冲突
	f, ok := cli.VersionFlag.(*cli.BoolFlag)
	if ok {
		f.Aliases = nil
	}

	return app
}

// RunPlugin initializes and runs the GPU kubelet plugin.
// RunPlugin 初始化并运行GPU kubelet插件，负责核心运行逻辑，设置环境、创建驱动、处理信号等
func RunPlugin(ctx context.Context, config *Config) error {
	// 启动调试信号处理器，通常用于捕获特定信号（如SIGUSR1）来输出调试信息或goroutine堆栈
	common.StartDebugSignalHandlers()

	// Create the plugin directory
	// 创建插件工作目录，用于存储socket文件、检查点、nvidia-cdi-hook等
	// 权限0750表示所有者可读写执行，组可读执行，其他用户无权限
	err := os.MkdirAll(config.DriverPluginPath(), 0750)
	if err != nil {
		return err
	}

	// Setup nvidia-cdi-hook binary
	// 设置nvidia-cdi-hook二进制文件，如果命令行未指定路径，则从容器镜像中复制到插件目录
	// nvidia-cdi-hook在容器启动时执行，用于设置GPU设备权限
	if err := config.setNvidiaCDIHookPath(); err != nil {
		return fmt.Errorf("error setting up nvidia-cdi-hook: %w", err)
	}

	// Initialize CDI root directory
	// 初始化CDI根目录，CDI规范文件会生成到这个目录，容器运行时会读取这些文件
	info, err := os.Stat(config.flags.cdiRoot)
	switch {
	case err != nil && os.IsNotExist(err):
		err := os.MkdirAll(config.flags.cdiRoot, 0750)
		if err != nil {
			return err
		}
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("path for cdi file generation is not a directory: '%v'", config.flags.cdiRoot)
	}

	// 创建一个可取消的context，监听多个终止信号
	// SIGHUP: 挂起信号，通常用于重新加载配置
	// SIGINT: 中断信号（Ctrl+C）
	// SIGTERM: 终止信号，这是Kubernetes停止容器时发送的标准信号
	// SIGQUIT: 退出信号（Ctrl+\），通常还会生成core dump
	// 这些信号都应该触发优雅关闭流程
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer cancel() // 确保context被取消，释放相关资源

	// Create and start the driver
	// 创建并启动DRA驱动
	// driver封装DRA插件的核心功能：NodePrepareResources、NodeUnprepareResources等
	// 这个调用会启动gRPC服务器、向kubelet注册插件、启动资源监控等
	driver, err := NewDriver(ctx, config)
	if err != nil {
		return fmt.Errorf("error creating driver: %w", err)
	}

	// 阻塞等待context被取消（收到终止信号）
	// 这使得插件保持运行状态，持续处理资源分配请求
	// 直到收到信号或context被父级取消
	<-ctx.Done()
	
	// 检查context取消的原因
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		// A canceled context is the normal case here when the process receives
		// a signal. Only log the error for more interesting cases.
		// context.Canceled是正常的优雅关闭，不需要记录错误
		// 其他错误（如DeadlineExceeded）则需要记录，可能表示异常情况
		klog.Errorf("error from context: %v", err)
	}

	// 优雅关闭驱动
	// 这会停止gRPC服务器、注销插件、清理资源等
	// 确保所有进行中的操作完成，避免资源泄漏
	err = driver.Shutdown()
	if err != nil {
		// 关闭失败可能导致资源泄漏，但此时已无法恢复
		// 记录错误便于事后分析
		klog.Errorf("unable to cleanly shutdown driver: %v", err)
	}

	return nil
}

// change to config
// If 'f.nvidiaCDIHookPath' is already set (from the command line), do nothing.
// If 'f.nvidiaCDIHookPath' is empty, it copies the nvidia-cdi-hook binary from
// /usr/bin/nvidia-cdi-hook to DriverPluginPath and sets 'f.nvidiaCDIHookPath'
// to this path. The /usr/bin/nvidia-cdi-hook is present in the current
// container image because it is copied from the toolkit image into this
// container at build time.
//
// setNvidiaCDIHookPath 设置nvidia-cdi-hook可执行文件的路径
// 1. 如果用户通过命令行指定路径，直接使用（返回nil）
// 2. 如果未指定，从容器镜像中复制nvidia-cdi-hook到插件目录
//
// - nvidia-cdi-hook必须在宿主机文件系统中可访问，因为容器运行时在宿主机上执行它
func (c Config) setNvidiaCDIHookPath() error {
	// 如果已经设置路径（通过命令行或环境变量），不需要做任何事
	if c.flags.nvidiaCDIHookPath != "" {
		return nil
	}

	// 源路径：容器镜像中nvidia-cdi-hook的位置
	// 这个文件在镜像构建时从NVIDIA Container Toolkit镜像复制而来
	sourcePath := "/usr/bin/nvidia-cdi-hook"
	
	// 目标路径：插件目录中的nvidia-cdi-hook
	// 这个目录通过hostPath卷挂载，所以文件会出现在宿主机文件系统中，对宿主机的容器运行时可见
	targetPath := filepath.Join(c.DriverPluginPath(), "nvidia-cdi-hook")

	// 读取源文件的全部内容
	// 使用ReadFile而不是Copy是为确保文件完整性和原子性
	input, err := os.ReadFile(sourcePath)
	if err != nil {
		return fmt.Errorf("error reading nvidia-cdi-hook: %w", err)
	}

	// 写入目标位置，权限0755表示可执行
	// 所有者可读写执行，组和其他用户可读执行
	// 这个权限是必需的，因为容器运行时需要执行这个hook
	if err := os.WriteFile(targetPath, input, 0755); err != nil {
		return fmt.Errorf("error copying nvidia-cdi-hook: %w", err)
	}

	// 更新配置，这个路径会被写入CDI规范文件，告诉容器运行时hook的位置
	c.flags.nvidiaCDIHookPath = targetPath

	return nil
}
