; Absorb 阅读助手 Windows 安装包脚本
; 编译: ISCC.exe installer\setup.iss
; 产物: build\bin\absorb-agent-setup-{版本}-{架构}.exe（架构经 /DMyAppArch 传入，缺省 x64）

#define MyAppName "Absorb 阅读助手"
#define MyAppNameEn "absorb-agent"
#ifndef MyAppVersion
#define MyAppVersion "0.1.0"
#endif
#ifndef MyAppArch
#define MyAppArch "x64"
#endif
#define MyAppExeName "absorb-agent-desktop.exe"

[Setup]
AppId={{508D84D4-03B7-4D5F-9AF4-40BB2DC0E384}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppVerName={#MyAppName} {#MyAppVersion}
DefaultDirName={autopf}\{#MyAppNameEn}
DisableProgramGroupPage=yes
WizardStyle=modern
Compression=lzma2
SolidCompression=yes
ArchitecturesInstallIn64BitMode=x64compatible
PrivilegesRequired=admin
; 升级期间用 Windows Restart Manager 关闭占用待更新文件的进程，并阻止其在安装期间重启。
; 注意：Restart Manager 管不到"浏览器扩展在宿主进程死亡后重新拉起同一 exe"这种跨进程重生，
; 那类占用由 [Code] 的 WaitAppGone 重试兜底（见文件末尾说明）。
CloseApplications=yes
CloseApplicationsFilter=absorb-agent-desktop.exe
UninstallDisplayName={#MyAppName}
UninstallDisplayIcon={app}\{#MyAppExeName}
SetupIconFile=..\build\windows\icon.ico
OutputDir=..\build\bin
OutputBaseFilename=absorb-agent-setup-{#MyAppVersion}-{#MyAppArch}

[Languages]
Name: "chinese"; MessagesFile: "compiler:Languages\ChineseSimplified.isl"

[Files]
Source: "..\build\bin\{#MyAppExeName}"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; Check: DesktopIconChecked

; 自启动不放在 [Registry] 节：PrivilegesRequired=admin 时 [Registry] 的 HKCU
; 指向执行提权的账户（官方文档），用别的管理员账户提权安装会把自启动写进
; 错误的用户。改为 [Code] 的 SetOriginalUserAutostart 经 ExecAsOriginalUser
; 用 reg.exe 落到发起安装的原用户 hive（见 ssPostInstall / usUninstall）。

[Run]
; Native Messaging 宿主注册（exe 内生成 native-host.json + 写 HKCU 双浏览器注册表）。
; runasoriginaluser 保证 HKCU 写到发起安装的用户；静默安装同样执行。
Filename: "{app}\{#MyAppExeName}"; Parameters: "--register-native-host"; Flags: runasoriginaluser runhidden skipifdoesntexist
Filename: "{app}\{#MyAppExeName}"; Description: "{cm:LaunchProgram,{#MyAppName}}"; Flags: nowait postinstall skipifsilent

[Code]
const
  AppExe = 'absorb-agent-desktop.exe';
  AppRegValue = 'absorb-agent';

var
  DelUserData: Boolean;
  CbDesktop, CbAutoStart: TCheckBox;
  GOptPage: TWizardPage;
  InstallSucceeded: Boolean;
  UnregisteredHost: Boolean;

// InitializeWizard 在"选择安装位置"后追加原生控件的附加选项页。
// 引擎自绘的任务列表(TNewCheckListBox)在 150% DPI 下复选框左边框被裁切，
// 故弃用 [Tasks]，改用原生 TCheckBox 渲染；控件高度须 ScaleY，否则高 DPI 下文字上下被裁。
procedure InitializeWizard();
var
  Info: TLabel;
begin
  GOptPage := CreateCustomPage(wpSelectDir, '选择附加任务', '您想要安装程序执行哪些附加任务？');

  Info := TLabel.Create(GOptPage);
  Info.Parent := GOptPage.Surface;
  Info.Left := ScaleX(4);
  Info.Top := ScaleY(24);
  Info.Caption := '选择您想要安装程序在安装 Absorb 阅读助手 时执行的附加任务，然后点击"下一步"。';

  CbDesktop := TCheckBox.Create(GOptPage);
  CbDesktop.Parent := GOptPage.Surface;
  CbDesktop.Left := ScaleX(4);
  CbDesktop.Top := ScaleY(56);
  CbDesktop.Width := ScaleX(380);
  CbDesktop.Height := ScaleY(24);
  CbDesktop.Checked := True;
  CbDesktop.Caption := '创建桌面快捷方式(D)';

  CbAutoStart := TCheckBox.Create(GOptPage);
  CbAutoStart.Parent := GOptPage.Surface;
  CbAutoStart.Left := ScaleX(4);
  CbAutoStart.Top := ScaleY(96);
  CbAutoStart.Width := ScaleX(380);
  CbAutoStart.Height := ScaleY(24);
  CbAutoStart.Checked := False;
  CbAutoStart.Caption := '开机自动启动（后台待命，划词即用）';
end;

function DesktopIconChecked(): Boolean;
begin
  Result := CbDesktop.Checked;
end;

function AutoStartChecked(): Boolean;
begin
  Result := CbAutoStart.Checked;
end;

// 进入附加选项页把默认焦点交给"下一步"按钮：避免焦点虚线框贴住复选框文字。
procedure CurPageChanged(CurPageID: Integer);
begin
  if CurPageID = GOptPage.ID then
    WizardForm.ActiveControl := WizardForm.NextButton;
end;

// SetOriginalUserAutostart 以"发起安装/卸载的原用户"身份写/删 HKCU Run 键。
// 替代原 [Registry] 节：提权运行时 [Registry] 的 HKCU 指向提升账户（见上节注释）。
// reg.exe 走标准 argv 解析，/d 里的 \" 还原为字面引号，落库值即带引号的完整路径。
procedure SetOriginalUserAutostart(Enable: Boolean);
var
  rc: Integer;
begin
  if Enable then begin
    ExecAsOriginalUser(ExpandConstant('{sys}\reg.exe'),
      '/add "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v ' + AppRegValue +
      ' /t REG_SZ /d "\"' + ExpandConstant('{app}\') + AppExe + '\"" /f',
      '', SW_HIDE, ewWaitUntilTerminated, rc);
    if rc <> 0 then
      Log('写入自启动注册表失败，reg.exe 退出码 ' + IntToStr(rc));
  end else begin
    // 值不存在或已被用户手动清除时 reg delete 返回非 0，属预期，忽略。
    ExecAsOriginalUser(ExpandConstant('{sys}\reg.exe'),
      '/delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v ' + AppRegValue + ' /f',
      '', SW_HIDE, ewWaitUntilTerminated, rc);
  end;
end;

// KillApp 结束桌面进程（/T 连带 WebView2 子进程树），避免文件占用残留。
procedure KillApp;
var
  rc: Integer;
begin
  Exec(ExpandConstant('{sys}\taskkill.exe'), '/F /T /IM ' + AppExe, '', SW_HIDE, ewWaitUntilTerminated, rc);
end;

// AppStillRunning 探测是否仍有桌面端进程在跑。
// 用 tasklist | find 的退出码判定（find 命中返回 0），无需解析临时文件。
function AppStillRunning: Boolean;
var
  rc: Integer;
begin
  Exec(ExpandConstant('{cmd}'),
    '/c tasklist /FI "IMAGENAME eq ' + AppExe + '" /NH | find /I "' + AppExe + '" > nul',
    '', SW_HIDE, ewWaitUntilTerminated, rc);
  Result := (rc = 0);
end;

// WaitAppGone 反复结束进程并等待其真正退出，返回 True 表示已无残留进程。
//
// 为什么需要重试：浏览器扩展与桌面端经 Native Messaging 通信，宿主进程死亡会导致
// 管道断裂、扩展立即重新拉起**同一个 exe**（宿主与 UI 是同一文件），单次 taskkill
// 根本无法释放文件锁——真机升级时表现为"尝试替换现有文件时出错：DeleteFile 失败，
// 错误代码 5（拒绝访问）"。这里用"杀 → 等 → 探"循环把占用让出来。
//
// attempts 为轮数、每轮间隔 1s；耗尽仍占用则返回 False，由调用方中止安装
// （明确失败远好过弹"重试/忽略"对话框后留下半升级状态）。
function WaitAppGone(attempts: Integer): Boolean;
var
  i: Integer;
begin
  for i := 1 to attempts do begin
    KillApp;
    if not AppStillRunning then begin
      Result := True;
      exit;
    end;
    Sleep(1000);
  end;
  Result := not AppStillRunning;
end;

// UnregisterNativeHostForUpgrade 升级前注销 Native Messaging 注册（Chrome/Edge 双注册表 + manifest）。
//
// 这是根因修复：宿主进程被杀后，扩展会按 5s 定时器重新 connectNative 拉起**同一个 exe**
// （extension/background/native-connection.js 的 _onDisconnect），把待替换的文件重新锁住，
// 表现为"DeleteFile 失败；错误代码 5（拒绝访问）"。而扩展对 "No such native application"
// 这一错误**不调度重连**——注销后重连必然失败，宿主不会被拉起，文件锁得以真正释放。
// 安装收尾由 [Run] 的 --register-native-host 重新注册。
procedure UnregisterNativeHostForUpgrade;
var
  rc: Integer;
begin
  if not ExecAsOriginalUser(ExpandConstant('{app}\{#MyAppExeName}'), '--unregister-native-host',
      ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, rc) then
    Log('升级前注销 Native Messaging 宿主失败（忽略，由 WaitAppGone 兜底）');
end;

// ShowUninstallOptions 卸载选项窗体：勾选决定卸载后是否清理用户数据。
// 返回 True = 开始卸载，False = 取消。
// 与安装页同一教训：控件 Height/Width 必须显式 ScaleX/ScaleY，否则高 DPI 下
// 文字上下被裁；横向尺寸用 ScaleX。
function ShowUninstallOptions(): Boolean;
var
  Form: TForm;
  Info: TLabel;
  Cb: TCheckBox;
  BtnGo, BtnNo: TButton;
begin
  DelUserData := False;
  Result := False;

  Form := TForm.Create(nil);
  try
    Form.Caption := '卸载 Absorb 阅读助手';
    Form.Position := poScreenCenter;
    Form.BorderStyle := bsDialog;
    Form.ClientWidth := ScaleX(400);
    Form.ClientHeight := ScaleY(158);

    Info := TLabel.Create(Form);
    Info.Parent := Form;
    Info.Left := ScaleX(16);
    Info.Top := ScaleY(14);
    Info.Caption := '应用组件即将被移除。';

    Cb := TCheckBox.Create(Form);
    Cb.Parent := Form;
    Cb.Left := ScaleX(16);
    Cb.Top := ScaleY(44);
    Cb.Width := ScaleX(368);
    Cb.Height := ScaleY(20); // 单行文字
    Cb.Checked := False;
    Cb.Caption := '同时删除用户数据（会话记录、模型配置、日志）';

    BtnGo := TButton.Create(Form);
    BtnGo.Parent := Form;
    BtnGo.Caption := '开始卸载';
    BtnGo.ModalResult := mrOk;
    BtnGo.Top := ScaleY(122);
    BtnGo.Left := ScaleX(160);
    BtnGo.Width := ScaleX(80);
    BtnGo.Height := ScaleY(28);

    BtnNo := TButton.Create(Form);
    BtnNo.Parent := Form;
    BtnNo.Caption := '取消';
    BtnNo.ModalResult := mrCancel;
    BtnNo.Top := ScaleY(122);
    BtnNo.Left := ScaleX(254);
    BtnNo.Width := ScaleX(80);
    BtnNo.Height := ScaleY(28);

    if Form.ShowModal() = mrOk then
    begin
      DelUserData := Cb.Checked;
      Result := True;
    end;
  finally
    Form.Free();
  end;
end;

// 覆盖安装前先结束旧进程（尽早让出占用）；真正的保证在 CurStepChanged(ssInstall)。
function InitializeSetup(): Boolean;
begin
  KillApp;
  Result := True;
end;

// 复制文件前必须真正释放 exe：先注销宿主注册（让扩展重连失败、不再拉起宿主），
// 再反复结束进程直至无残留。拿不到文件锁就明确中止——远好过弹"重试/忽略"对话框
// 后留下半升级状态。
procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssInstall then
  begin
    UnregisterNativeHostForUpgrade;
    // 已注销标记：无论后续成败（含 WaitAppGone 失败中止），退出时都需恢复注册
    UnregisteredHost := True;
    if not WaitAppGone(20) then
      RaiseException('无法结束正在运行的 Absorb 阅读助手，安装已中止。' + #13#10 +
        '请先完全退出浏览器（桌面端退出后扩展会重新拉起它），然后重新运行安装程序。');
  end;
  if CurStep = ssPostInstall then
  begin
    InstallSucceeded := True;
    // 勾选了"开机自动启动"则写原用户 Run 键（替代原 [Registry] 节的 uninsdeletevalue 方案）
    if AutoStartChecked then
      SetOriginalUserAutostart(True);
  end;
end;

// 安装未成功完成时恢复宿主注册：卸载/中止发生在 ssInstall 之后的话，
// 扩展此时应仍能连上桌面端，不能因为一次失败的升级而失去 Native Messaging。
//
// 仅在真正注销过注册后才恢复——用户在目录页等早期阶段取消时，{app} 常量
// 尚未初始化，ExpandConstant 会抛内部错误（"expand the app constant before
// it was initialized"）；且此阶段从未动过注册表，本就无需恢复。
procedure DeinitializeSetup();
var
  rc: Integer;
begin
  if InstallSucceeded or (not UnregisteredHost) then
    exit;
  ExecAsOriginalUser(ExpandConstant('{app}\{#MyAppExeName}'), '--register-native-host',
    ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, rc);
end;

// 卸载开始前询问数据清理选项；点"取消"中止卸载。静默卸载跳过（保留数据）。
function InitializeUninstall(): Boolean;
begin
  Result := True;
  if not UninstallSilent then
    Result := ShowUninstallOptions();
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usUninstall then
  begin
    // 卸载也必须先注销 Native Messaging（与升级路径同一竞态）：宿主被杀后
    // 扩展按 5s 定时器重连拉起同一 exe，反复锁住待删文件——曾致卸载器卡死
    // 数小时（unins000/_unins.tmp 残留）。注销后重连必然失败，复活链断掉。
    UnregisterNativeHostForUpgrade;
    WaitAppGone(20);
    // 清理自启动 Run 键（原 [Registry] 节 uninsdeletevalue 的等价替代，删不到属预期）
    SetOriginalUserAutostart(False);
  end;
  if CurUninstallStep = usPostUninstall then
  begin
    if DelUserData then
    begin
      DelTree(ExpandConstant('{userappdata}\{#MyAppNameEn}'), True, True, True);
      DelTree(ExpandConstant('{userappdata}\') + AppExe, True, True, True);
    end;
  end;
end;
