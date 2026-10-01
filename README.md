# Onerclone

把夸克网盘挂成 Windows **本地磁盘**：文件以「占位符」形式出现在资源管理器里，打开时才按需下载（Cloud Files API），本地改动自动回传。单个 Go exe，无驱动安装、无管理员权限。

```
资源管理器 / 记事本
      │  读/写
      ▼
占位符（NTFS · Cloud Files API）── FETCH_DATA ──► onerclone.exe
                                                     │  rclone RC
                                                     ▼
                                                夸克网盘 quark:
```

## 环境要求

- Windows 10 1709+（Cloud Files API，实测 Win10 LTSC 21H2 / build 19041 ✓）
- 同步根所在盘必须是 **NTFS**
- `rclone.exe`（**夸克专用 fork**：`v1.70.0-quark`，含 quark backend + RC）
  放到 `onerclone.exe` 同目录即可被自动发现

## 快速开始

```powershell
# 1) 扫码登录夸克（只需一次；cookie 过期后重跑）
.\onerclone.exe quark-login

# 2) 开始同步（无参数 = 打开 GUI 窗口，同步引擎随窗口启动）
#    在终端里跑 `onerclone run` 可用 CLI 形态（输出接回当前终端）
.\onerclone.exe
```

想用夸克以外的存储（Dropbox / OneDrive / WebDAV / SFTP…）？

```powershell
.\onerclone.exe login              # 列出全部可登录后端
.\onerclone.exe login -type dropbox # 通用登录（OAuth 自动开浏览器；扫码后端出二维码）
# 然后把 onerclone.json 的 remote 改成 dropbox: 并重启
```

启动后：

- **托盘图标**：右键 → 打开同步根 / 打开日志 / 退出
- 登录、换 remote、换同步根走 CLI 或直接改 `onerclone.json`
- 资源管理器打开同步根即可看到云朵图标

## 安装（推荐）

双击 `onerclone-setup-<版本>.exe` → 安装到 `%LOCALAPPDATA%\Onerclone`（**无需管理员**），
安装时可勾选：桌面快捷方式、**开机自启**、装完立即启动。

或免安装：解压 zip 后直接运行 `onerclone.exe`。

## 管理方式

Onerclone 不再启动本地 Web 面板。日常状态看 Explorer 的云朵/绿勾图标；传输队列/失败重试看 GUI 窗口（无参数启动）或 `onerclone.log`；登录用 `onerclone quark-login` 或 `onerclone login`；配置直接编辑 `onerclone.json` 后重启。

## 配置（exe 同目录 `onerclone.json`）

首次运行自动生成；**命令行 flag > 配置文件 > 内置默认**。

```json
{
  "sync_root": "C:\\Users\\你\\Onerclone",
  "remote": "quark:",
  "rclone": "",
  "offline": false
}
```

| 字段 | 含义 | 默认 |
|---|---|---|
| `sync_root` | 本地同步根（必须 NTFS） | `%USERPROFILE%\Onerclone` |
| `remote` | 真实 rclone remote；留空 = 本地替身模式（自动生成示例数据，用于离线自测） | `quark:` |
| `fs` | 本地替身目录（仅 `remote` 为空时用） | `./spike-remote` |
| `rclone` | rclone 路径；留空 = 自动发现（exe 同目录 → PATH → 兜底） | 自动 |
| `offline` | 离线模式：水合请求立即快速失败 | `false` |

## 命令

```text
onerclone                    直接开始同步（读 onerclone.json）
onerclone run [flags]        同上，flag 覆盖配置
                             -root -remote -fs -rclone -offline
onerclone quark-login        扫码登录夸克（需真实控制台；-tries N 超时自动重出二维码）
onerclone login [-type T]    通用登录任意 rclone 后端（省略 -type 列出全部可选）
onerclone autostart          查询开机自启；-enable/-disable 开关（HKCU，无需 UAC）
onerclone register   [-root] 注册同步根（普通用户即可，无需 UAC）
onerclone unregister [-root] 注销同步根
onerclone version            版本信息
onerclone help               帮助
```

日志双写：控制台 + `onerclone.log`（exe 同目录）。

## 行为说明（实测结论）

- **状态图标语义**：入队 = 同步中（合箭头/云朵）；动作结算且该路径无任何未完成动作 = 绿勾；失败/重试中保持同步中。目录不预标绿勾（目录绿勾由启动对账 RestoreInSync 统一裁决），避免“子文件还没传完目录先绿”的乱显示
- **退出后**：已标绿的文件保持绿勾（in-sync 是 NTFS 元数据，不随进程消失）；同步根整体显示“未连接”属预期（与 OneDrive 退出后同款）

- **离线读**：未水合文件立即报错（5ms，不卡死不崩）；已水合文件离线可读
- **冲突**：双方都改 → 本地版留原名上传 + 云端版存 `xxx (冲突 时间).txt`，两边各保留一份
- **断线**：rclone rcd 掉线 → 5s 起指数退避自动重启（封顶 30s，无限重试），队列自动挂起/恢复
- **cookie 过期**：队列按「认证失败」停摆并在日志提示重新扫码
- **云端非法字符名**（如 `来自:分享`）：本地自动转义成 `来自%3A分享`，云端名保持原样
- **本地全角冒号等 rclone 读不到的名**（`：？＜＞｜＊＼＂`——rclone 在 Windows 会把它们
  改写成 ASCII 再打开，必然 404）：文件/目录自动**改名**成 `%XX` 转义名（磁盘 = 云端，字面一致）；
  `（）`、`Ａ` 等 rclone 读得到的字符**不改名**，本地保持原样（云端名是登记过的转义名）
- **删除**：本地删 → 删云端；基线未建立前一律保护（DR2）

## 从源码构建

```powershell
.\build.ps1 -Version 0.4.0                 # 测试 + 构建 + 打包 → dist\*.zip
.\build.ps1 -Version 0.4.0 -Installer      # 额外编译安装包（需 Inno Setup 6）
```

产物：

- `dist\onerclone-v<版本>-win64\` —— 发布目录（`onerclone.exe` + `rclone.exe` + README + VERSION）
- `dist\onerclone-v<版本>-win64.zip` —— 压缩包（可直接分发）
- `dist\onerclone-setup-<版本>.exe` —— 安装包（`-Installer`；缺 Inno 时：`winget install --id JRSoftware.InnoSetup --scope user`）

## 已知边界

- 托盘图标只有一个静态样式（无“在线/离线”状态区分）；菜单动作见上
- CLI 扫码若中途关掉程序，会自动把原 cookie 写回（登录态不丢）；扫描成功前不要重复跑 `quark-login`
- `rclone.exe` 必须是夸克 fork；官方 rclone 没有 quark backend（用其他后端时官方版亦可）
- 换同步根：改 `onerclone.json` 后重启——自动注销旧根注册、迁移到新根；状态库在 exe 同目录，队列/基线不丢
- 换 remote：改 `onerclone.json` 后重启——云端基线自动作废重建（防误删）；换根不等于换 remote，两者独立
- 卸载：控制面板/设置里卸载 Onerclone（会关自启并注销同步根；正在运行时注销会失败，属预期）
