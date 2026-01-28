# NVIDIA DRA Driver 完整工作流程指南

本文档详细说明从插件部署、资源发布到用户申请资源的完整 DRA 工作流程，重点关注逻辑流程和函数调用链关系。

---

## 目录

1. [总体架构](#总体架构)
2. [完整流程图](#完整流程图)
3. [Checkpoint 机制详解](#checkpoint-机制详解)
4. [文件职责说明](#文件职责说明)
5. [阶段 1: 插件启动与资源发布](#阶段-1-插件启动与资源发布)
6. [阶段 2: 用户申请资源（Scheduler 分配）](#阶段-2-用户申请资源scheduler-分配)
7. [阶段 3: Kubelet 准备设备（Prepare）](#阶段-3-kubelet-准备设备prepare)
8. [阶段 4: 容器启动](#阶段-4-容器启动)
9. [阶段 5: Unprepare（清理）](#阶段-5-unprepare清理)
10. [阶段 6: 健康监控（可选）](#阶段-6-健康监控可选)

---

## 总体架构

```
┌─────────────────────────────────────────────────────────────┐
│                    Kubernetes 集群                           │
│                                                              │
│  ┌──────────┐    ┌──────────┐    ┌──────────┐             │
│  │Scheduler │◄──►│API Server│◄──►│ Kubelet  │             │
│  └──────────┘    └──────────┘    └─────┬────┘             │
│                                         │ gRPC             │
└─────────────────────────────────────────┼───────────────────┘
                                          │
                    ┌─────────────────────▼──────────────────┐
                    │   NVIDIA DRA Kubelet Plugin            │
                    │                                         │
                    │  ┌──────────┐  ┌────────────┐         │
                    │  │  Driver  │  │DeviceState │         │
                    │  └──────────┘  └────────────┘         │
                    │                                         │
                    │  Managers: CDI, MPS, TimeSlicing, VFIO │
                    └─────────────────┬──────────────────────┘
                                      │ NVML
                    ┌─────────────────▼──────────────────────┐
                    │        NVIDIA Driver + GPU             │
                    └────────────────────────────────────────┘
```

---

## Checkpoint 机制详解

### 1. 为什么需要 Checkpoint？

在 Kubernetes 中，Kubelet 可能会在任何时候重启。如果没有持久化的状态记录，Kubelet 重启后会丢失所有关于"哪些设备已经分配给哪些 Pod"的信息。这会导致严重问题：

1.  **资源泄漏**：Kubelet 忘记已分配的资源，可能再次将其分配给其他 Pod，导致冲突。
2.  **状态不一致**：Pod 正在运行使用 GPU，但插件认为该 GPU 是空闲的。
3.  **无法清理**：当 Pod 终止时，插件不知道该 Pod 占用了哪些资源，无法正确执行清理（Unprepare）操作。

因此，**Checkpoint（检查点）** 机制是 DRA 插件可靠性的基石。它确保了资源分配状态在插件重启、Kubelet 重启甚至节点重启后依然持久存在。

### 2. Kubelet Checkpoint 通用流程

Kubernetes 提供了通用的 `checkpointmanager` 工具库，标准流程如下：

1.  **Prepare 开始**：插件收到 Prepare 请求，记录"开始准备"状态到磁盘。
2.  **执行准备**：插件执行实际的硬件配置（如划分 MIG、设置时间片）。
3.  **Prepare 完成**：插件记录"准备完成"状态及分配详情到磁盘。
4.  **Unprepare 开始**：插件收到 Unprepare 请求，读取 Checkpoint 确认该设备确实被该 Claim 占用。
5.  **执行清理**：插件执行硬件清理操作。
6.  **Unprepare 完成**：插件从 Checkpoint 中删除该记录。

### 3. 本项目中的 Checkpoint 实现

在 `k8s-dra-driver-gpu` 中，Checkpoint 数据结构定义在 `cmd/gpu-kubelet-plugin/checkpoint.go` 中，核心逻辑如下：

#### 数据结构

```go
// 存储在 /var/lib/kubelet/plugins_registry/gpu.nvidia.com/checkpoint.json
type CheckpointV2 struct {
    PreparedClaims map[string]PreparedClaimV2
}

type PreparedClaimV2 struct {
    // 状态机：PrepareStarted -> PrepareCompleted
    CheckpointState ClaimCheckpointState
    // 包含了分配给该 Claim 的具体设备列表、配置等
    PreparedDevices PreparedDevices
}
```

#### 关键流程

**A. 插件启动 (Startup)**
- `NewDeviceState` 调用 `checkpointManager.ListCheckpoints()`
- 如果发现 `checkpoint.json`，加载到内存中的 `DeviceState`。
- 如果没有，创建一个新的空 Checkpoint。
- **作用**：恢复上次运行时的状态，知道哪些 GPU 已经被占用了。

**B. 准备资源 (Prepare)**
1.  **幂等性检查**：
    - 在执行任何操作前，先检查 Checkpoint。
    - 如果 `CheckpointState == PrepareCompleted`，说明之前已经成功准备过了（可能是 Kubelet 重试），直接返回缓存的 `PreparedDevices`。
    - **收益**：避免重复执行昂贵的硬件配置操作。

2.  **两阶段提交**：
    - **阶段 1**：更新 Checkpoint 为 `PrepareStarted`。如果此时崩溃，重启后知道有一个未完成的准备操作。
    - **执行操作**：配置 GPU、生成 CDI 文件。
    - **阶段 2**：更新 Checkpoint 为 `PrepareCompleted` 并保存 `PreparedDevices`。
    - **收益**：确保状态的一致性。

**C. 释放资源 (Unprepare)**
1.  **存在性检查**：
    - 检查 Checkpoint 中是否有该 Claim UID。
    - 如果没有，说明可能从未准备过，或者已经清理过了，直接返回成功（幂等性）。

2.  **执行清理**：
    - 使用 Checkpoint 中保存的 `PreparedDevices` 信息来执行清理（因为此时 ResourceClaim 对象可能已经从 API Server 删除了，Checkpoint 是唯一的信息来源）。
    - 恢复 GPU 设置、删除 CDI 文件。

3.  **移除记录**：
    - 从 Checkpoint 中删除该 Claim 的记录，并刷写到磁盘。

#### Checkpoint 状态机

```
┌─────────────────────────────────────────────────────────────────────────────────────────────────┐
│                           Checkpoint 状态机                                                     │
└─────────────────────────────────────────────────────────────────────────────────────────────────┘

    ┌─────────────────────────────────────────────────────────────────────────────────────────┐
    │                                                                                         │
    │  ┌──────────────┐                                                                       │
    │  │   不存在     │  ← 初始状态，从未准备过该 Claim                                        │
    │  └──────┬───────┘                                                                       │
    │         │                                                                               │
    │         │ Prepare() 调用                                                                │
    │         ▼                                                                               │
    │  ┌──────────────┐                                                                       │
    │  │PrepareStarted│  ← 已记录准备开始，但未完成。如果崩溃，重启后需要检查是否需要清理。    │
    │  └──────┬───────┘                                                                       │
    │         │                                                                               │
    │         │ 准备成功                                                                       │
    │         ▼                                                                               │
    │  ┌──────────────────┐                                                                   │
    │  │PrepareCompleted  │  ← 准备完成，包含完整的设备列表和配置。幂等性检查点。              │
    │  └────────┬─────────┘                                                                   │
    │           │                                                                               │
    │           │ Unprepare() 调用                                                             │
    │           ▼                                                                               │
    │  ┌──────────────┐                                                                       │
    │  │   不存在     │  ← 清理完成，记录被删除。                                              │
    │  └──────────────┘                                                                       │
    │                                                                                         │
    └─────────────────────────────────────────────────────────────────────────────────────────┘
```

#### Checkpoint 详细流程

```
┌─────────────────────────────────────────────────────────────────────────────────────────────────┐
│                           Checkpoint 详细流程                                                   │
└─────────────────────────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────────────────────────┐
│ 场景 1: 正常 Prepare 流程                                                                       │
├─────────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                                 │
│  Kubelet 调用 PrepareResourceClaims(claim)                                                      │
│    │                                                                                            │
│    ├─► state.Prepare(claim)                                                                    │
│    │   │                                                                                         │
│    │   ├─► getCheckpoint() [从磁盘读取 checkpoint.json]                                         │
│    │   │   └─► checkpointManager.GetCheckpoint("checkpoint.json")                               │
│    │   │                                                                                         │
│    │   ├─► 幂等性检查 [检查是否已准备]                                                          │
│    │   │   └─► if checkpoint.PreparedClaims[claimUID].CheckpointState == PrepareCompleted:     │
│    │   │       └─► 返回缓存的 PreparedDevices (跳过实际准备)                                    │
│    │   │                                                                                         │
│    │   ├─► updateCheckpoint() [标记 PrepareStarted]                                            │
│    │   │   ├─► checkpoint.PreparedClaims[claimUID] = {                                         │
│    │   │   │       CheckpointState: PrepareStarted,                                           │
│   │   │   │       Status: claim.Status                                                        │
│    │   │   │   }                                                                               │
│    │   │   └─► checkpointManager.CreateCheckpoint("checkpoint.json", checkpoint)               │
│    │   │       └─► 写入磁盘: /var/lib/kubelet/plugins_registry/gpu.nvidia.com/checkpoint.json   │
│    │   │                                                                                         │
│    │   ├─► prepareDevices(claim) [执行实际准备]                                                │
│    │   │   ├─► 解析配置 (GpuConfig, MigConfig, VfioConfig)                                     │
│    │   │   ├─► 应用配置 (设置时间片、启动 MPS、配置 VFIO)                                      │
│    │   │   └─► 生成 PreparedDevices                                                            │
│    │   │                                                                                         │
│    │   ├─► cdi.CreateClaimSpecFile(claimUID, preparedDevices) [生成 CDI 文件]                   │
│    │   │   └─► 写入: /etc/cdi/nvidia-claim-{uid}.yaml                                          │
│    │   │                                                                                         │
│    │   └─► updateCheckpoint() [标记 PrepareCompleted]                                          │
│    │       ├─► checkpoint.PreparedClaims[claimUID] = {                                         │
│    │       │       CheckpointState: PrepareCompleted,                                         │
│    │       │       Status: claim.Status,                                                      │
│    │       │       PreparedDevices: preparedDevices  ← 保存完整结果                           │
│    │       │   }                                                                               │
│    │       └─► checkpointManager.CreateCheckpoint("checkpoint.json", checkpoint)               │
│    │           └─► 写入磁盘 (持久化)                                                           │
│    │                                                                                             │
│    └─► 返回 PreparedDevices 给 Kubelet                                                         │
│                                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────────────────────────┐
│ 场景 2: 插件崩溃后重启 (幂等性)                                                                  │
├─────────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                                 │
│  插件崩溃                                                                                       │
│    │                                                                                            │
│    ├─► Kubelet 检测到插件无响应                                                                  │
│    │                                                                                             │
│    ├─► Kubelet 重启插件                                                                         │
│    │                                                                                             │
│    └─► NewDeviceState() [插件重新启动]                                                          │
│        │                                                                                         │
│        ├─► checkpointManager.ListCheckpoints() [列出所有 checkpoint 文件]                       │
│        │   └─► 发现: checkpoint.json                                                            │
│        │                                                                                         │
│        ├─► checkpointManager.GetCheckpoint("checkpoint.json") [加载 checkpoint]                 │
│        │   └─► 读取磁盘上的持久化状态                                                           │
│        │       └─► PreparedClaims: {                                                           │
│        │           "claim-uid-1": {                                                            │
│        │               CheckpointState: PrepareCompleted,                                      │
│        │               PreparedDevices: [...]                                                  │
│        │           }                                                                            │
│        │       }                                                                                │
│        │                                                                                         │
│        └─► 恢复状态到内存中的 DeviceState                                                       │
│            └─► 现在插件知道 claim-uid-1 已经占用了哪些设备                                      │
│                                                                                                 │
│  Kubelet 重试 PrepareResourceClaims(claim-uid-1) [因为之前的调用超时]                            │
│    │                                                                                            │
│    └─► state.Prepare(claim-uid-1)                                                              │
│        │                                                                                         │
│        ├─► getCheckpoint() [读取 checkpoint]                                                   │
│        │   └─► 发现 claim-uid-1 的状态是 PrepareCompleted                                       │
│        │                                                                                         │
│        ├─► 幂等性检查 [跳过实际准备]                                                            │
│        │   └─► 返回缓存的 PreparedDevices                                                      │
│        │       └─► 不再执行硬件配置，直接返回之前保存的结果                                     │
│        │                                                                                         │
│        └─► 返回给 Kubelet [快速响应]                                                            │
│                                                                                                 │
│  结果：设备配置不会重复执行，避免了资源冲突和性能浪费。                                           │
│                                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────────────────────────┐
│ 场景 3: 正常 Unprepare 流程                                                                      │
├─────────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                                 │
│  Kubelet 调用 UnprepareResourceClaims(claimRef)                                                 │
│    │                                                                                            │
│    └─► state.Unprepare(claimUID)                                                               │
│        │                                                                                         │
│        ├─► getCheckpoint() [读取 checkpoint]                                                   │
│        │   └─► checkpointManager.GetCheckpoint("checkpoint.json")                               │
│        │                                                                                         │
│        ├─► 检查 claim 是否存在                                                                   │
│        │   ├─► if claimUID not in checkpoint.PreparedClaims:                                   │
│        │   │   └─► 返回 nil (幂等性：从未准备过或已清理)                                        │
│        │   │                                                                                     │
│        │   └─► 获取 PreparedClaim                                                               │
│        │       └─► pc = checkpoint.PreparedClaims[claimUID]                                    │
│        │                                                                                         │
│        ├─► 检查 CheckpointState                                                                 │
│        │   ├─► if pc.CheckpointState == PrepareStarted:                                        │
│        │   │   └─► 返回 nil (未完成准备，无需清理)                                              │
│        │   │                                                                                     │
│        │   └─► if pc.CheckpointState == PrepareCompleted:                                      │
│        │       └─► 继续清理流程                                                                │
│        │                                                                                         │
│        ├─► unprepareDevices(claimUID, pc.PreparedDevices) [执行清理]                            │
│        │   ├─► 使用 pc.PreparedDevices 中的信息 (因为 ResourceClaim 可能已删除)                  │
│        │   ├─► 停止 MPS 守护进程                                                                │
│        │   ├─► 恢复默认时间片设置                                                               │
│        │   ├─► 解绑 VFIO 驱动                                                                  │
│        │   └─► 恢复兄弟设备                                                                    │
│        │                                                                                         │
│        ├─► cdi.DeleteClaimSpecFile(claimUID) [删除 CDI 文件]                                    │
│        │   └─► 删除: /etc/cdi/nvidia-claim-{uid}.yaml                                          │
│        │                                                                                         │
│        └─► 从 checkpoint 删除记录                                                               │
│            ├─► delete(checkpoint.PreparedClaims, claimUID)                                     │
│            └─► checkpointManager.CreateCheckpoint("checkpoint.json", checkpoint)               │
│                └─► 写入磁盘 (持久化删除)                                                       │
│                                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────────────────────────┐
│ 场景 4: Unprepare 时插件崩溃                                                                     │
├─────────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                                 │
│  Kubelet 调用 UnprepareResourceClaims(claimRef)                                                 │
│    │                                                                                            │
│    └─► state.Unprepare(claimUID)                                                               │
│        │                                                                                         │
│        ├─► getCheckpoint() [读取 checkpoint]                                                   │
│        │   └─► 发现 claimUID 存在，状态是 PrepareCompleted                                      │
│        │                                                                                         │
│        ├─► unprepareDevices() [开始清理]                                                       │
│        │   ├─► 停止 MPS 守护进程                                                                │
│        │   └─► 插件崩溃!                                                                        │
│        │                                                                                         │
│  插件重启                                                                                       │
│    │                                                                                            │
│    └─► NewDeviceState() [重新启动]                                                             │
│        │                                                                                         │
│        └─► 加载 checkpoint [恢复状态]                                                           │
│            └─► claimUID 仍然在 checkpoint 中，状态是 PrepareCompleted                           │
│                                                                                                 │
│  Kubelet 重试 UnprepareResourceClaims(claimRef)                                                 │
│    │                                                                                            │
│    └─► state.Unprepare(claimUID)                                                               │
│        │                                                                                         │
│        ├─► getCheckpoint() [读取 checkpoint]                                                   │
│        │   └─► 发现 claimUID 存在，状态是 PrepareCompleted                                      │
│        │                                                                                         │
│        ├─► unprepareDevices() [重新执行清理]                                                   │
│        │   ├─► 停止 MPS 守护进程 (幂等：如果已停止则跳过)                                        │
│        │   ├─► 恢复默认时间片设置                                                               │
│        │   ├─► 解绑 VFIO 驱动                                                                  │
│        │   └─► 恢复兄弟设备                                                                    │
│        │                                                                                         │
│        ├─► cdi.DeleteClaimSpecFile(claimUID) [删除 CDI 文件]                                    │
│        │   └─► 删除: /etc/cdi/nvidia-claim-{uid}.yaml                                          │
│        │                                                                                         │
│        └─► 从 checkpoint 删除记录                                                               │
│            └─► checkpointManager.CreateCheckpoint("checkpoint.json", checkpoint)               │
│                └─► 写入磁盘 (持久化删除)                                                       │
│                                                                                                 │
│  结果：即使崩溃，资源也能被正确清理，不会泄漏。                                                 │
│                                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────────────────────┘
```

#### Checkpoint 文件位置

```
/var/lib/kubelet/plugins_registry/gpu.nvidia.com/checkpoint.json
```

#### Checkpoint 文件示例

```json
{
  "checksum": 1234567890,
  "v2": {
    "checksum": 9876543210,
    "preparedClaims": {
      "claim-uid-1": {
        "checkpointState": "PrepareCompleted",
        "status": {
          "allocation": {
            "devices": {
              "results": [
                {
                  "request": "gpu-request",
                  "driver": "gpu.nvidia.com",
                  "pool": "node1",
                  "device": "gpu-0"
                }
              ]
            }
          }
        },
        "preparedDevices": [
          {
            "devices": [
              {
                "gpu": {
                  "info": {
                    "uuid": "GPU-xxx",
                    "minor": 0,
                    "memoryBytes": 42949672960
                  },
                  "device": {
                    "requests": ["gpu-request"],
                    "poolName": "node1",
                    "deviceName": "gpu-0",
                    "cDIDeviceIDs": ["k8s.gpu.nvidia.com/device=gpu-0"]
                  }
                }
              }
            ],
            "configState": {
              "mpsControlDaemonID": "mps-daemon-claim-uid-1"
            }
          }
        ]
      }
    }
  }
}
```

#### Checkpoint 关键设计要点

1.  **幂等性**：Prepare 和 Unprepare 操作都是幂等的，可以安全地重试。
2.  **两阶段提交**：PrepareStarted -> PrepareCompleted，确保状态的一致性。
3.  **持久化**：所有状态变更都立即写入磁盘，确保崩溃后可以恢复。
4.  **校验和**：每个版本都有校验和，防止数据损坏。
5.  **版本兼容**：支持 V1 和 V2 版本，可以平滑升级和降级。

---

## 文件职责说明

### 核心文件

| 文件名 | 职责描述 | 主要类型/函数 |
|--------|----------|---------------|
| **main.go** | 程序入口，负责 CLI 参数解析、配置初始化和驱动启动 | `main()`, `RunPlugin()`, `Flags` |
| **driver.go** | DRA Plugin 主驱动，实现 Kubelet Plugin 接口，协调各组件 | `driver`, `NewDriver()`, `PrepareResourceClaims()`, `UnprepareResourceClaims()` |
| **device_state.go** | 设备状态管理，负责 Prepare/Unprepare 逻辑和 Checkpoint 管理 | `DeviceState`, `Prepare()`, `Unprepare()`, `prepareDevices()`, `unprepareDevices()` |

### 设备枚举与信息

| 文件名 | 职责描述 | 主要类型/函数 |
|--------|----------|---------------|
| **nvlib.go** | NVIDIA 设备库封装，提供 NVML 和 PCI 设备枚举接口 | `deviceLib`, `newDeviceLib()`, `enumerateAllPossibleDevices()` |
| **deviceinfo.go** | 设备信息结构体定义，包含 GPU、MIG、VFIO 设备的详细信息 | `GpuInfo`, `MigDeviceInfo`, `VfioDeviceInfo`, `HealthStatus` |
| **allocatable.go** | 可分配设备集合管理，提供设备类型转换和查询接口 | `AllocatableDevice`, `AllocatableDevices`, `Type()`, `GetDevice()` |

### CDI 与设备准备

| 文件名 | 职责描述 | 主要类型/函数 |
|--------|----------|---------------|
| **cdi.go** | CDI 规范文件生成和管理，负责设备注入配置 | `CDIHandler`, `NewCDIHandler()`, `CreateStandardDeviceSpecFile()`, `CreateClaimSpecFile()` |
| **prepared.go** | 已准备设备的数据结构定义，用于存储 Prepare 结果 | `PreparedDevice`, `PreparedDeviceGroup`, `PreparedDevices` |
| **cdioptions.go** | CDI 处理器的配置选项，使用 functional options 模式 | `WithNvml()`, `WithDeviceLib()`, `WithDriverRoot()` |

### 资源共享与配置

| 文件名 | 职责描述 | 主要类型/函数 |
|--------|----------|---------------|
| **sharing.go** | GPU 资源共享管理，包括时间片和 MPS（多进程服务） | `TimeSlicingManager`, `MpsManager`, `MpsControlDaemon`, `SetTimeSlice()`, `Start()`, `Stop()` |
| **vfio-device.go** | VFIO 直通设备管理，负责驱动绑定和解绑 | `VfioPciManager`, `NewVfioPciManager()`, `ConfigureDevices()`, `UnconfigureDevices()` |

### 状态持久化

| 文件名 | 职责描述 | 主要类型/函数 |
|--------|----------|---------------|
| **checkpoint.go** | Checkpoint 数据结构定义，支持版本化和校验和 | `Checkpoint`, `CheckpointV1`, `CheckpointV2`, `MarshalCheckpoint()`, `UnmarshalCheckpoint()` |
| **checkpointv.go** | Checkpoint 版本转换逻辑，支持 V1 到 V2 的升级 | `ToV2()`, `ToV1()` |

### 健康检查

| 文件名 | 职责描述 | 主要类型/函数 |
|--------|----------|---------------|
| **health.go** | gRPC 健康检查服务，供 Kubernetes 探针使用 | `healthcheck`, `startHealthcheck()`, `Check()` |
| **device_health.go** | 设备健康监控，基于 NVML 事件检测硬件故障 | `nvmlDeviceHealthMonitor`, `newNvmlDeviceHealthMonitor()`, `Start()`, `Unhealthy()` |

### 工具与配置

| 文件名 | 职责描述 | 主要类型/函数 |
|--------|----------|---------------|
| **types.go** | 通用类型定义和常量 | `Config`, `OpaqueDeviceConfig`, `DeviceConfigState` |
| **root.go** | 驱动根路径管理，用于定位驱动文件和工具 | `root`, `getDriverLibraryPath()`, `getNvidiaSMIPath()`, `getDevRoot()` |
| **mutex.go** | 互斥锁封装，用于并发控制 | `flock.Flock`, `Acquire()`, `Release()` |

### 文件职责分类

#### 1. **启动与初始化**
- `main.go` - 程序入口
- `root.go` - 路径管理
- `types.go` - 配置类型

#### 2. **设备枚举与发现**
- `nvlib.go` - NVML/PCI 设备库
- `deviceinfo.go` - 设备信息结构
- `allocatable.go` - 可分配设备集合

#### 3. **DRA 核心逻辑**
- `driver.go` - DRA Plugin 主驱动
- `device_state.go` - 设备状态管理

#### 4. **设备准备与配置**
- `prepared.go` - 已准备设备结构
- `sharing.go` - 时间片和 MPS 管理
- `vfio-device.go` - VFIO 直通管理

#### 5. **CDI 集成**
- `cdi.go` - CDI 规范生成
- `cdioptions.go` - CDI 配置选项

#### 6. **状态持久化**
- `checkpoint.go` - Checkpoint 数据结构
- `checkpointv.go` - 版本转换

#### 7. **健康监控**
- `health.go` - gRPC 健康检查
- `device_health.go` - 设备健康监控

#### 8. **并发控制**
- `mutex.go` - 文件锁

---

## 阶段 1: 插件启动与资源发布

### 1.1 完整启动流程

```
main() [程序入口]
  │
  ├─► 解析命令行参数
  │   └─ 获取 nodeName, pluginPath, cdiRoot 等配置
  │
  ├─► NewDriver(ctx, config) [创建驱动实例]
  │   │
  │   ├─► NewDeviceState(ctx, config) [初始化设备状态]
  │   │   │
  │   │   ├─► newDeviceLib(containerDriverRoot) [初始化 NVML 库]
  │   │   │   └─ 建立与 NVIDIA 驱动的连接
  │   │   │
  │   │   ├─► nvdevlib.enumerateAllPossibleDevices(config) [枚举所有设备]
  │   │   │   ├─► enumerateGpus() [发现完整 GPU]
  │   │   │   │   └─ 查询 NVML 获取所有物理 GPU
  │   │   │   │
  │   │   │   ├─► enumerateMigDevices() [发现 MIG 设备]
  │   │   │   │   └─ 查询已配置的 MIG 实例
  │   │   │   │
  │   │   │   └─► enumerateVfioDevices() [发现 VFIO 设备]
  │   │   │       └─ 查询可用于直通的 GPU
  │   │   │
  │   │   ├─► NewCDIHandler(options...) [创建 CDI 处理器]
  │   │   │   └─ 初始化 CDI 文件生成器
  │   │   │
  │   │   ├─► NewTimeSlicingManager(nvdevlib) [创建时间片管理器]
  │   │   │   └─ 用于 GPU 时间共享配置
  │   │   │
  │   │   ├─► NewMpsManager(config, ...) [创建 MPS 管理器]
  │   │   │   └─ 用于多进程服务配置
  │   │   │
  │   │   ├─► NewVfioPciManager(...) [创建 VFIO 管理器]
  │   │   │   └─ 用于 GPU 直通配置
  │   │   │
  │   │   ├─► cdi.CreateStandardDeviceSpecFile(allocatable) [生成标准 CDI 文件]
  │   │   │   └─ 为所有设备生成基础 CDI 规范
  │   │   │
  │   │   └─► checkpointManager.CreateCheckpoint() [初始化 Checkpoint]
  │   │       └─ 创建或恢复持久化状态
  │   │
  │   ├─► kubeletplugin.Start(ctx, driver, options...) [启动 Kubelet Plugin]
  │   │   ├─ 创建 gRPC 服务器
  │   │   ├─ 注册到 kubelet (通过 unix socket)
  │   │   └─ 启动资源发布循环
  │   │
  │   ├─► startHealthcheck(ctx, config) [启动健康检查服务]
  │   │   └─ 提供 HTTP 端点供 Kubernetes 探针使用
  │   │
  │   ├─► deviceHealthMonitor.Start(ctx) [启动设备健康监控]
  │   │   └─ 监控 GPU 硬件状态（XID 错误等）
  │   │
  │   └─► publishResources(ctx, config) [发布资源到 Kubernetes]
  │       │
  │       ├─► 遍历 state.allocatable [获取所有可分配设备]
  │       │   └─ device.GetDevice() [转换为 Kubernetes Device 对象]
  │       │
  │       ├─► 构建 resourceslice.DriverResources [构建资源结构]
  │       │   └─ 组织设备到 Pool 中
  │       │
  │       └─► pluginhelper.PublishResources(ctx, resources) [发布到 API Server]
  │           └─ 创建/更新 ResourceSlice 对象
  │
  └─► 等待信号并调用 Shutdown() [优雅关闭]
```

### 1.2 发布的 ResourceSlice（用户视角）

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
metadata:
  name: node1-gpu-nvidia-com-abc123
spec:
  nodeName: node1
  driver: gpu.nvidia.com
  pool:
    name: node1
  devices:
  - name: gpu-0                              # ← 来自 GpuInfo.CanonicalName()
                                                 # ← 格式: "gpu-{minor}"，其中 minor 是 GPU 设备号
                                                 # ← 来源: NVML 查询获取的 GPU minor number
    attributes:
      type: {string: "gpu"}                  # ← 来自 GpuInfo.GetDevice()
                                                 # ← 固定值 "gpu" 表示完整 GPU 设备
      uuid: {string: "GPU-xxx"}              # ← 来自 GpuInfo.UUID
                                                 # ← 来源: NVML 查询获取的 GPU UUID
      productName: {string: "A100-SXM4-40GB"} # ← 来自 GpuInfo.productName
                                                 # ← 来源: NVML 查询获取的 GPU 产品名称
      architecture: {string: "Ampere"}       # ← 来自 GpuInfo.architecture
                                                 # ← 来源: NVML 查询获取的 GPU 架构代号
      cudaComputeCapability: {version: "8.0"} # ← 来自 GpuInfo.cudaComputeCapability
                                                 # ← 来源: NVML 查询获取的 CUDA 计算能力
    capacity:
      memory: {value: "42949672960"}         # ← 来自 GpuInfo.memoryBytes
                                                 # ← 来源: NVML 查询获取的 GPU 显存大小（字节）
```

---

## 阶段 2: 用户申请资源（Scheduler 分配）

### 2.1 用户创建 ResourceClaim

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: my-gpu-claim
spec:
  devices:
    requests:
    - name: gpu-request                      # ← 请求名称
      deviceClassName: nvidia-gpu
      selectors:                             # ← CEL 表达式选择器
      - cel:
          expression: |
            device.attributes["architecture"] == "Ampere" &&
            device.capacity["memory"] >= quantity("32Gi")
      count: 1                               # ← 请求数量
    config:                                  # ← Opaque 配置
    - opaque:
        driver: gpu.nvidia.com
        parameters:
          apiVersion: gpu.nvidia.com/v1beta1
          kind: GpuConfig
          sharing:
            strategy: TimeSlicing
```

### 2.2 调度器分配流程

```
Scheduler [调度器]
  │
  ├─► 监听 Pod 创建事件
  │   └─ 检测到 Pod 引用 ResourceClaim "my-gpu-claim"
  │
  ├─► 读取 ResourceSlice [获取所有节点的可用设备]
  │   └─ 从 API Server 获取所有节点的设备列表
  │
  ├─► 应用选择器过滤 [筛选符合条件的节点]
  │   ├─ 评估 CEL 表达式
  │   │   └─ device.attributes["architecture"] == "Ampere"
  │   └─ 检查容量
  │       └─ device.capacity["memory"] >= 32Gi
  │
  ├─► 选择最佳节点 [调度决策]
  │   └─ 选择 node1 (假设)
  │
  └─► 更新 ResourceClaim.status [记录分配结果]
      └─ 在 status.allocation 中写入分配信息

补充说明：
调度器只负责选择节点和设备，不执行实际的设备准备。
实际的设备准备由 Kubelet 在节点上执行，通过调用 DRA Plugin 完成。
device_state.go 中的 Prepare() 和 Unprepare() 方法在调度完成后才被调用。
```

### 2.3 分配后的 ResourceClaim

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: my-gpu-claim
status:
  allocation:
    devices:
      results:
      - request: gpu-request               # ← 对应 spec.requests[0].name
        driver: gpu.nvidia.com
        pool: node1                        # ← 分配的节点
        device: gpu-0                      # ← 分配的设备
      config:
      - source: FromClaim
        opaque:
          driver: gpu.nvidia.com
          parameters:
            apiVersion: gpu.nvidia.com/v1beta1
            kind: GpuConfig
            sharing:
              strategy: TimeSlicing
```

---

## 阶段 3: Kubelet 准备设备（Prepare）

### 3.1 Prepare 完整流程

```
Kubelet [Pod 调度到本节点]
  │
  ├─► 检测到 Pod.spec.resourceClaims 引用 "my-gpu-claim"
  │   └─ Kubelet 内置的 DRA 控制器监控 Pod 状态
  │
  ├─► 读取 ResourceClaim [获取分配信息]
  │   └─ 从 API Server 读取 status.allocation.devices.results
  │
  └─► 调用 DRA Plugin [通过 gRPC]
      │
      └─ Kubelet 通过之前在 main() 中启动的 gRPC 服务器调用 PrepareResourceClaims
          │
          └─► driver.nodePrepareResource(ctx, claim) [处理单个 claim]
              │
              ├─► pulock.Acquire(ctx, 10s) [获取互斥锁]
              │   └─ 防止 Prepare/Unprepare 并发执行
              │
              ├─► state.Prepare(ctx, claim) [准备设备]
              │   │
              │   ├─► getCheckpoint() [读取 checkpoint]
              │   │   └─ 获取已准备的 claim 状态，实现幂等性
              │   │
              │   ├─► 幂等性检查 [避免重复准备]
              │   │   └─ 如果已 PrepareCompleted，返回缓存结果
              │   │
              │   ├─► updateCheckpoint() [标记 PrepareStarted]
              │   │   └─ 记录准备开始状态到 checkpoint
              │   │
              │   ├─► prepareDevices(ctx, claim) [执行设备准备]
              │   │   │
              │   │   ├─► GetOpaqueDeviceConfigs() [解析配置]
              │   │   │   └─ 从 claim.status.allocation.devices.config 解析
              │   │   │
              │   │   ├─► 配置映射 [为每个设备找到对应配置]
              │   │   │   └─ 遍历 results，匹配 config.Requests
              │   │   │
              │   │   ├─► for each config: [应用每个配置]
              │   │   │     applyConfig(ctx, config, claim, results)
              │   │   │       │
              │   │   │       ├─► GpuConfig: [GPU 配置]
              │   │   │       │   ├─ tsManager.SetTimeSlice() [设置时间片]
              │   │   │       │   │   └─ 调用 NVML 设置 GPU 时间片参数
              │   │   │       │   │
              │   │   │       │   └─ mpsManager.Start() [启动 MPS]
              │   │   │       │       └─ 启动 MPS 控制守护进程
              │   │   │       │
              │   │   │       ├─► MigDeviceConfig: [MIG 配置]
              │   │   │       │   └─ applyMigConfig() [应用 MIG 配置]
              │   │   │       │
              │   │   │       └─► VfioDeviceConfig: [VFIO 配置]
              │   │   │           └─ vfioPciManager.ConfigureDevices()
              │   │   │               ├─ 解绑 nvidia 驱动
              │   │   │               └─ 绑定 vfio-pci 驱动
              │   │   │
              │   │   └─► 构建 PreparedDevices [生成准备结果]
              │   │       ├─ 收集设备信息
              │   │       ├─ 生成 CDI 设备 ID
              │   │       └─ 返回准备好的设备列表
              │   │
              │   ├─► Passthrough 模式处理 [移除兄弟设备]
              │   │   └─ allocatable.RemoveSiblingDevices()
              │   │       └─ 从可分配列表中移除同一 GPU 的其他类型设备
              │   │
              │   ├─► cdi.CreateClaimSpecFile(claimUID, preparedDevices) [生成 CDI 文件]
              │   │   └─ 为该 claim 生成特定的 CDI 规范文件
              │   │
              │   └─► updateCheckpoint() [标记 PrepareCompleted]
              │       └─ 保存准备结果到 checkpoint，实现崩溃恢复
              │
              ├─► Passthrough: publishResources() [重新发布资源]
              │   └─ 更新 ResourceSlice（移除已分配的兄弟设备）
              │
              └─► pulock.Release() [释放互斥锁]
                  └─ 允许其他 Prepare/Unprepare 操作
```

---

## 阶段 4: 容器启动

### 4.1 容器运行时集成流程

```
Kubelet [接收 PrepareResult]
  │
  ├─► 获取 CDIDeviceIDs [从 PrepareResult]
  │   └─ ["nvidia.com/gpu=gpu-0", "nvidia.com/gpu=my-gpu-claim-gpu-0"]
  │
  ├─► 调用容器运行时（CRI）[创建容器]
  │   └─ CreateContainer(config)
  │       └─ config.devices.container_edits 包含 CDI 设备
  │
  └─► 容器运行时（如 containerd）[应用 CDI 规范]
      │
      ├─► 读取 CDI 规范文件
      │   ├─ /etc/cdi/nvidia-gpu.yaml          # 标准设备
      │   └─ /etc/cdi/nvidia-claim-{uid}.yaml  # Claim 特定配置
      │
      ├─► 应用 CDI 规范
      │   ├─ 挂载设备: /dev/nvidia0
      │   ├─ 设置环境变量: CUDA_VISIBLE_DEVICES=0
      │   └─ 执行钩子: nvidia-container-runtime-hook
      │
      └─► 启动容器
```

### 4.2 CDI 文件示例

```yaml
# /etc/cdi/nvidia-gpu.yaml (标准设备)
cdiVersion: 0.5.0
kind: nvidia.com/gpu
devices:
- name: gpu-0
  containerEdits:
    deviceNodes:
    - path: /dev/nvidia0
    - path: /dev/nvidiactl
    - path: /dev/nvidia-uvm

# /etc/cdi/nvidia-claim-{uid}.yaml (Claim 特定)
cdiVersion: 0.5.0
kind: nvidia.com/gpu
devices:
- name: my-gpu-claim-gpu-0
  containerEdits:
    env:
    - CUDA_VISIBLE_DEVICES=0
    - NVIDIA_VISIBLE_DEVICES=GPU-xxx
```

---

## 阶段 5: Unprepare（清理）

### 5.1 Unprepare 完整流程

```
Kubelet [Pod 终止]
  │
  └─► 调用 DRA Plugin [通过 gRPC]
      │
      └─ Kubelet 通过 gRPC 调用 UnprepareResourceClaims
          │
          └─► driver.nodeUnprepareResource(ctx, claimRef) [处理单个 claim]
              │
              ├─► pulock.Acquire(ctx, 10s) [获取互斥锁]
              │
              ├─► state.Unprepare(ctx, claimUID) [清理设备]
              │   │
              │   ├─► getCheckpoint() [读取 checkpoint]
              │   │   └─ 获取已准备的 claim 状态
              │   │
              │   ├─► 检查 checkpoint 状态 [幂等性检查]
              │   │   ├─ 不存在: 返回 nil (从未准备过)
              │   │   └─ PrepareStarted: 返回 nil (未完成准备)
              │   │
              │   ├─► unprepareDevices(ctx, claimUID, preparedDevices) [执行清理]
              │   │   │
              │   │   ├─► Passthrough: [VFIO 清理]
              │   │   │     unprepareVfioDevices()
              │   │   │       ├─ 解绑 vfio-pci 驱动
              │   │   │       └─ 重新绑定 nvidia 驱动
              │   │   │
              │   │   ├─► MPS: [MPS 清理]
              │   │   │     mpsControlDaemon.Stop()
              │   │   │       └─ 停止 MPS 控制守护进程
              │   │   │
              │   │   └─► TimeSlicing: [时间片清理]
              │   │         tsManager.SetTimeSlice(默认值)
              │   │           └─ 恢复默认时间片设置
              │   │
              │   ├─► Passthrough: [恢复兄弟设备]
              │   │     discoverSiblingAllocatables()
              │   │       ├─ 重新枚举设备
              │   │       └─ 添加回 MIG/VFIO 兄弟设备
              │   │
              │   ├─► cdi.DeleteClaimSpecFile(claimUID) [删除 CDI 文件]
              │   │   └─ 删除 claim 特定的 CDI 规范文件
              │   │
              │   └─► 从 checkpoint 删除记录 [清理状态]
              │       └─ 删除 PreparedClaims[claimUID]，实现幂等性
              │
              ├─► Passthrough: publishResources() [重新发布资源]
              │   └─ 更新 ResourceSlice（恢复兄弟设备）
              │
              └─► pulock.Release() [释放互斥锁]
                  └─ 允许其他 Prepare/Unprepare 操作
```

---

## 阶段 6: 健康监控（可选）

### 6.1 健康监控流程

```
deviceHealthMonitor.Start(ctx) [启动健康监控]
  │
  └─► 启动后台 goroutine [持续监控]
      │
      └─► for device in allocatable: [遍历所有设备]
            nvml.DeviceGetXidErrors(device) [检查 XID 错误]
            ├─ 检测到 XID 错误
            └─ 发送到 Unhealthy() channel

driver.deviceHealthEvents(ctx, nodeName) [处理健康事件]
  │
  └─► for unhealthyDevice := range monitor.Unhealthy(): [监听不健康设备]
        │
        ├─► state.UpdateDeviceHealthStatus(device, Unhealthy) [更新健康状态]
        │   └─ 标记设备为不健康
        │
        ├─► 构建新的 ResourceSlice [仅包含健康设备]
        │   └─ 过滤掉不健康的设备
        │
        └─► pluginhelper.PublishResources(ctx, resources) [重新发布]
            └─ 更新 API Server 上的 ResourceSlice
```

---