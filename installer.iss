; onerclone.iss —— Inno Setup 安装脚本（**普通用户安装，无需 UAC**）
;
; 编译（由 build.ps1 -Installer 调用，或手动）：
;   ISCC.exe /DMyAppVersion=0.2.0 installer.iss
; 产物: dist\onerclone-setup-<版本>.exe
;
; 安装行为：
;   - 目录：{localappdata}\Onerclone（可写 → 配置/日志都在旁边）
;   - 可选任务：桌面快捷方式 / 开机自启（调用 `onerclone autostart -enable`）
;   - 卸载：先关自启，再 `onerclone unregister` 注销同步根（读 {app} 配置）

#ifndef MyAppVersion
  #define MyAppVersion "0.3.6"
#endif
#define MyAppName "Onerclone"
#define MyAppExeName "onerclone.exe"
#define StageDir "dist\onerclone-v" + MyAppVersion + "-win64"

[Setup]
AppId=Onerclone
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher=Onerclone
DefaultDirName={localappdata}\{#MyAppName}
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
OutputDir=dist
OutputBaseFilename=onerclone-setup-{#MyAppVersion}
Compression=lzma2/max
SolidCompression=yes
SetupIconFile=cmd\spike\ui\tray.ico
UninstallDisplayIcon={app}\{#MyAppExeName}
UninstallDisplayName={#MyAppName}
WizardStyle=modern
ArchitecturesInstallIn64BitMode=x64compatible
ArchitecturesAllowed=x64compatible

[Languages]
Name: "chs"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "创建桌面快捷方式"; Flags: unchecked
Name: "autostart"; Description: "开机自动启动（写入 HKCU Run，普通用户权限）"
Name: "launch"; Description: "安装完成后立即启动 {#MyAppName}"

[Files]
Source: "{#StageDir}\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs

[Icons]
Name: "{autoprograms}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; Tasks: desktopicon

[Run]
; 开机自启（仅在勾选任务时执行）
Filename: "{app}\{#MyAppExeName}"; Parameters: "autostart -enable"; \
    StatusMsg: "正在配置开机自启…"; Flags: runhidden; Tasks: autostart
; 立即启动
Filename: "{app}\{#MyAppExeName}"; Description: "启动 {#MyAppName}"; \
    Flags: nowait postinstall skipifsilent; Tasks: launch

[UninstallRun]
; 卸载时先关掉开机自启（best-effort，失败不阻断）
Filename: "{app}\{#MyAppExeName}"; Parameters: "autostart -disable"; \
    RunOnceId: "DisableAutostart"; Flags: runhidden
; 注销同步根（读 {app}\onerclone.json；程序在跑时会失败，属预期）
Filename: "{app}\{#MyAppExeName}"; Parameters: "unregister"; \
    RunOnceId: "UnregisterSyncRoot"; Flags: runhidden

[UninstallDelete]
; 运行期产物：日志与状态库（配置保留，重装可续用）
Type: files; Name: "{app}\onerclone.log"
Type: filesandordirs; Name: "{app}\OnercloneSpike.state"
