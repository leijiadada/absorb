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
; 捆绑 poppler（runtime/poppler：pdftotext + 依赖 DLL + share/poppler 数据），
; 供本地 file:// PDF 提取（extract.pdftotextBin 探测 {app}\runtime\poppler）
Source: "..\runtime\poppler\*"; DestDir: "{app}\runtime\poppler"; Flags: ignoreversion recursesubdirs

[Icons]
Name: "{autoprograms}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; Check: DesktopIconChecked

; 自启动写/删 Run 键走 [Code] 而非 [Registry]：admin 提权时 [Registry] 的 HKCU 指向提权账户，
; 跨账户安装会写错用户。安装侧用 ExecAsOriginalUser 落到原用户 hive；卸载侧该函数被引擎禁用，
; 只能以卸载器身份 Exec 删除（跨账户提权卸载时原用户键会残留，属 Windows 权限模型固有限制）。

[Run]
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
  InUninstaller: Boolean;

// 在"选择安装位置"后追加原生控件附加选项页。
// 引擎自绘的 TNewCheckListBox 在 150% DPI 下复选框左边框被裁切，改用原生 TCheckBox；
// 控件 Height/Width 须显式 ScaleX/ScaleY，否则高 DPI 下文字被裁。
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

// 进入附加选项页把焦点交给"下一步"按钮，避免焦点虚线框贴住复选框文字。
procedure CurPageChanged(CurPageID: Integer);
begin
  if CurPageID = GOptPage.ID then
    WizardForm.ActiveControl := WizardForm.NextButton;
end;

// 写/删 HKCU Run 键。Enable=True 安装时调用，Enable=False 卸载时调用。
// reg.exe argv 解析 /d 内的 \" 为字面引号，落库值即带引号的完整路径。
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
    // 值不存在或已被用户手动清除时 reg delete 返回非 0，忽略。
    Exec(ExpandConstant('{sys}\reg.exe'),
      '/delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v ' + AppRegValue + ' /f',
      '', SW_HIDE, ewWaitUntilTerminated, rc);
  end;
end;

// 结束桌面进程（/T 连带子进程树）。
procedure KillApp;
var
  rc: Integer;
begin
  Exec(ExpandConstant('{sys}\taskkill.exe'), '/F /T /IM ' + AppExe, '', SW_HIDE, ewWaitUntilTerminated, rc);
end;

// 探测桌面进程是否仍在运行（tasklist | find 命中返回 0）。
function AppStillRunning: Boolean;
var
  rc: Integer;
begin
  Exec(ExpandConstant('{cmd}'),
    '/c tasklist /FI "IMAGENAME eq ' + AppExe + '" /NH | find /I "' + AppExe + '" > nul',
    '', SW_HIDE, ewWaitUntilTerminated, rc);
  Result := (rc = 0);
end;

// 反复结束进程并等待退出。浏览器扩展在宿主被杀后会重新拉起同一个 exe，
// 单次 taskkill 无法释放文件锁；此处"杀 → 等 → 探"循环耗尽仍占用则返回 False。
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

// 升级/卸载前注销 Native Messaging 注册（Chrome/Edge 双注册表 + manifest）。
// 扩展对"No such native application"不调度重连，注销后宿主不会被重新拉起，
// 文件锁得以真正释放。安装收尾由 [Run] 的 --register-native-host 重新注册。
// InUninstaller 为 True 时 ExecAsOriginalUser 被引擎禁用，改用 Exec。
procedure UnregisterNativeHostForUpgrade;
var
  rc: Integer;
begin
  if InUninstaller then
  begin
    if not Exec(ExpandConstant('{app}\{#MyAppExeName}'), '--unregister-native-host',
        ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, rc) then
      Log('卸载前注销 Native Messaging 宿主失败（忽略，由 WaitAppGone 兜底）');
    exit;
  end;
  if not ExecAsOriginalUser(ExpandConstant('{app}\{#MyAppExeName}'), '--unregister-native-host',
      ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, rc) then
    Log('升级前注销 Native Messaging 宿主失败（忽略，由 WaitAppGone 兜底）');
end;

// 卸载选项窗体：勾选决定是否清理用户数据。返回 True = 开始卸载，False = 取消。
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
    Cb.Height := ScaleY(20);
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

// 覆盖安装前先结束旧进程，尽早让出占用。
function InitializeSetup(): Boolean;
begin
  KillApp;
  Result := True;
end;

// 复制文件前注销宿主注册（让扩展重连失败、不再拉起宿主），再反复结束进程直至无残留。
// 拿不到文件锁则明确中止，避免留下半升级状态。
procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssInstall then
  begin
    UnregisterNativeHostForUpgrade;
    UnregisteredHost := True;
    if not WaitAppGone(20) then
      RaiseException('无法结束正在运行的 Absorb 阅读助手，安装已中止。' + #13#10 +
        '请先完全退出浏览器（桌面端退出后扩展会重新拉起它），然后重新运行安装程序。');
  end;
  if CurStep = ssPostInstall then
  begin
    InstallSucceeded := True;
    if AutoStartChecked then
      SetOriginalUserAutostart(True);
  end;
end;

// 安装未成功完成时恢复宿主注册，避免因一次失败升级丢失 Native Messaging 能力。
// UnregisteredHost 为 False 说明从未动过注册表（用户在早期阶段取消），无需恢复；
// 且此时 {app} 尚未初始化，ExpandConstant 会抛内部错误。
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
// InUninstaller 标志在此置位，后续卸载步骤据此分流 ExecAsOriginalUser 的替代实现。
function InitializeUninstall(): Boolean;
begin
  InUninstaller := True;
  Result := True;
  if not UninstallSilent then
    Result := ShowUninstallOptions();
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usUninstall then
  begin
    UnregisterNativeHostForUpgrade;
    WaitAppGone(20);
    SetOriginalUserAutostart(False);
  end;
  if CurUninstallStep = usPostUninstall then
  begin
    if DelUserData then
    begin
      // 数据根 = %APPDATA%\absorb-agent；便携模式数据在安装目录内，随目录删除一并清除。
      DelTree(ExpandConstant('{userappdata}\{#MyAppNameEn}'), True, True, True);
    end;
  end;
end;
