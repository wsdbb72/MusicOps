; MusicOps Go installer (Inno Setup 6.5+).
; Build with: ISCC /DAppVersion=<version> /DBundleDir=<audited Go bundle folder> /O<output folder> MusicOps-go.iss
; The bundle is the Go bundle produced by packaging/build_windows_go.ps1:
; MusicOps.exe (Wails desktop shell) + musicops.exe (Go core) + tools/ + docs.
; There is no Python runtime in this bundle: no _internal, no MusicOps-worker.exe.
; AppVersion is passed on the command line by the release workflow; the
; default below is only a dev fallback and must never ship a release.
#ifndef BundleDir
  #define BundleDir "..\dist\MusicOps-Windows"
#endif
#ifndef AppIdentity
  #define AppIdentity "MusicOps.Desktop"
#endif
#ifndef AppVersion
  #define AppVersion "0.0.0-dev"
#endif

[Setup]
AppId={#AppIdentity}
AppName=MusicOps
AppVersion={#AppVersion}
AppPublisher=wsdbb72
AppPublisherURL=https://github.com/wsdbb72/MusicOps
AppSupportURL=https://github.com/wsdbb72/MusicOps/issues
DefaultDirName={code:GetInstallDirectory}
UsePreviousAppDir=no
DisableDirPage=no
DisableWelcomePage=no
DefaultGroupName=MusicOps
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
WizardStyle=modern
DisableProgramGroupPage=yes
AllowNoIcons=yes
OutputBaseFilename=MusicOps-{#AppVersion}-Setup
SetupIconFile={#SourcePath}..\src\musicops\assets\musicops.ico
Compression=lzma2
SolidCompression=yes
CloseApplications=no
RestartApplications=no
UninstallDisplayIcon={app}\MusicOps.exe
VersionInfoVersion={#AppVersion}.0
VersionInfoDescription=MusicOps Windows Installer

[CustomMessages]
chinesesimp.UpgradeTitle=更新 MusicOps
chinesesimp.UpgradeWelcome=已检测到现有 MusicOps%1。%n%n安装程序将在原目录更新应用，保留配置、已保存的凭据、任务记录和媒体工具。%n%n更新前请在任务中心使用“退出应用及后台”。如有任务正在运行，请先等待当前阶段结束，或安全取消需退出的任务，再退出。%n%n更新目录：%2
chinesesimp.FreshWelcome=未检测到现有安装，将安装 MusicOps。%n%n如果您以前使用便携版，请在下一步选择原来的 MusicOps 目录；安装程序会将其识别为更新并保留 profile 和 tools。
chinesesimp.UpdateButton=更新(&U)
chinesesimp.UpdateSummary=更新现有 MusicOps%1 至 {#AppVersion}%n保留配置、凭据、任务记录和媒体工具。%n%n更新目录：
chinesesimp.DifferentDirectory=检测到现有 MusicOps 安装。为避免创建第二份程序和空白配置，请更新原目录：%n%1%n%n如需迁移，请先完整备份原目录，再卸载旧版本。
chinesesimp.NewerVersion=已安装的 MusicOps %1 比此安装包 {#AppVersion} 更新。请使用同版本或更新的安装包，避免旧程序读取新版数据。
chinesesimp.BusyApplication=MusicOps 或后台核心仍在运行，或安装文件暂时被占用。%n%n请先等待当前阶段结束，或安全取消需退出的任务，再在任务中心使用“退出应用及后台”。退出后点击“重试”。安装程序不会终止进程或取消任务。
english.UpgradeTitle=Update MusicOps
english.UpgradeWelcome=An existing MusicOps%1 installation was detected.%n%nSetup will update the original directory and preserve settings, saved credentials, tasks and media tools.%n%nWait for the current stage to finish, or safely cancel tasks that prevent exiting, then use "Quit app and background worker" in the task center.%n%nUpdate directory: %2
english.FreshWelcome=No existing installation was detected. Setup will install MusicOps.%n%nIf you previously used the portable edition, select its existing MusicOps directory on the next page. Setup will recognize it as an update and preserve profile and tools.
english.UpdateButton=&Update
english.UpdateSummary=Update existing MusicOps%1 to {#AppVersion}%nPreserve settings, credentials, tasks and media tools.%n%nUpdate directory:
english.DifferentDirectory=An existing MusicOps installation was detected. Update its original directory to avoid creating another copy and an empty profile:%n%1%n%nTo move the application, back up the original directory before uninstalling it.
english.NewerVersion=MusicOps %1 is newer than this {#AppVersion} installer. Use the same or a newer version to avoid opening newer data with an older application.
english.BusyApplication=MusicOps or its background core is still running, or an application file is temporarily in use.%n%nWait for the current stage to finish, or safely cancel tasks that prevent exiting, then use "Quit app and background worker" in the task center and choose Retry. Setup will not terminate processes or cancel tasks.

[Languages]
Name: "chinesesimp"; MessagesFile: "{#SourcePath}\Languages\ChineseSimplified.isl"
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"
Name: "autostart"; Description: "登录 Windows 后自动启动 MusicOps"; GroupDescription: "后台任务"; Flags: unchecked

[Registry]
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "MusicOps"; ValueData: """{app}\MusicOps.exe"" --profile ""{app}\profile"""; Tasks: autostart; Flags: uninsdeletevalue

[Dirs]
Name: "{app}\profile"; Flags: uninsneveruninstall

[Files]
Source: "{#BundleDir}\MusicOps.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BundleDir}\musicops.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BundleDir}\licenses\*"; DestDir: "{app}\licenses"; Flags: ignoreversion recursesubdirs createallsubdirs
Source: "{#BundleDir}\LICENSE"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BundleDir}\THIRD-PARTY-NOTICES.md"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BundleDir}\启动说明.txt"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BundleDir}\用户操作说明.md"; DestDir: "{app}"; Flags: ignoreversion skipifsourcedoesntexist
Source: "{#BundleDir}\运维与恢复说明.md"; DestDir: "{app}"; Flags: ignoreversion skipifsourcedoesntexist
Source: "{#BundleDir}\manifest.json"; DestDir: "{app}"; Flags: ignoreversion skipifsourcedoesntexist

Source: "{#BundleDir}\tools\ffmpeg.exe"; DestDir: "{app}\tools"; Flags: ignoreversion skipifsourcedoesntexist
Source: "{#BundleDir}\tools\ffprobe.exe"; DestDir: "{app}\tools"; Flags: ignoreversion skipifsourcedoesntexist
Source: "{#BundleDir}\tools\FFmpeg-version.txt"; DestDir: "{app}\tools"; Flags: ignoreversion skipifsourcedoesntexist
Source: "{#BundleDir}\tools\FFmpeg-LICENSE"; DestDir: "{app}\tools"; Flags: ignoreversion skipifsourcedoesntexist
Source: "{#BundleDir}\tools\FFmpeg-README.txt"; DestDir: "{app}\tools"; Flags: ignoreversion skipifsourcedoesntexist
Source: "{#BundleDir}\tools\services\*"; DestDir: "{app}\tools\services"; Flags: ignoreversion recursesubdirs createallsubdirs skipifsourcedoesntexist

[Icons]
Name: "{group}\MusicOps"; Filename: "{app}\MusicOps.exe"; Parameters: "--profile ""{app}\profile"""; WorkingDir: "{app}"
Name: "{autodesktop}\MusicOps"; Filename: "{app}\MusicOps.exe"; Parameters: "--profile ""{app}\profile"""; WorkingDir: "{app}"; Tasks: desktopicon

[Run]
Filename: "{app}\MusicOps.exe"; Parameters: "--profile ""{app}\profile"""; Description: "{cm:LaunchProgram,MusicOps}"; Flags: nowait postinstall skipifsilent

[Code]
{ Inno Setup 6 only builds 32-bit installers and has no NativeInt type.
  Inno Setup 7 supplies the correctly sized built-in type for either architecture.
  https://jrsoftware.org/ishelp/topic_64bit.htm }
type
#if Ver < EncodeVer(7, 0, 0)
  TMusicOpsNativeInt = LongInt;
  TMusicOpsRootKey = LongInt;
#else
  TMusicOpsNativeInt = NativeInt;
  TMusicOpsRootKey = HKEY;
#endif

const
  UninstallKey = 'Software\Microsoft\Windows\CurrentVersion\Uninstall\{#AppIdentity}_is1';
var
  ExistingDirectory, ExistingVersion, RequestedDirectory: String;
  DetectionComplete, UpgradeDetected: Boolean;

function CreateFileForUpdate(const FileName: String; DesiredAccess, ShareMode: Cardinal;
  SecurityAttributes: TMusicOpsNativeInt; CreationDisposition, Flags: Cardinal;
  TemplateFile: TMusicOpsNativeInt): TMusicOpsNativeInt;
  external 'CreateFileW@kernel32.dll stdcall';
function CloseFileHandle(Handle: TMusicOpsNativeInt): Boolean;
  external 'CloseHandle@kernel32.dll stdcall';

function NormalizeDirectory(Value: String): String;
begin
  Result := RemoveBackslashUnlessRoot(ExpandFileName(Value));
end;

{ A directory is an existing MusicOps install when it holds the Go shell
  plus the Go core, or the legacy Python shell plus its worker and
  _internal runtime. Both layouts upgrade in place so the profile,
  credentials and tools survive the Python-to-Go switch. }
function IsExistingMusicOps(const Directory: String): Boolean;
begin
  Result := (Directory <> '') and FileExists(AddBackslash(Directory) + 'MusicOps.exe') and
    (FileExists(AddBackslash(Directory) + 'musicops.exe') or
     (FileExists(AddBackslash(Directory) + 'MusicOps-worker.exe') and
      DirExists(AddBackslash(Directory) + '_internal')));
end;

function ReadRegisteredInstallation(Root: TMusicOpsRootKey): Boolean;
var
  Directory: String;
begin
  Result := RegQueryStringValue(Root, UninstallKey, 'InstallLocation', Directory) and
    IsExistingMusicOps(Directory);
  if Result then begin
    ExistingDirectory := NormalizeDirectory(Directory);
    RegQueryStringValue(Root, UninstallKey, 'DisplayVersion', ExistingVersion);
  end;
end;

function RequestedInstallDirectory: String;
var
  I: Integer;
  Argument: String;
begin
  Result := '';
  for I := 1 to ParamCount do begin
    Argument := ParamStr(I);
    if CompareText(Copy(Argument, 1, 5), '/DIR=') = 0 then
      Result := NormalizeDirectory(RemoveQuotes(Copy(Argument, 6, Length(Argument))));
  end;
end;

procedure DetectExistingInstallation;
var
  Directory: String;
  Shell, Shortcut: Variant;
begin
  if DetectionComplete then Exit;
  DetectionComplete := True;
  RequestedDirectory := RequestedInstallDirectory;
  { Inno's normal previous-directory handling does not cover portable copies.
    Registered installs remain authoritative; do not silently create a second copy. }
  if not ReadRegisteredInstallation(HKCU64) then ReadRegisteredInstallation(HKCU32);
  if ExistingDirectory = '' then begin
    if RequestedDirectory <> '' then begin
      if IsExistingMusicOps(RequestedDirectory) then ExistingDirectory := RequestedDirectory;
    end else begin
      Directory := ExpandConstant('{localappdata}\Programs\MusicOps');
      if IsExistingMusicOps(Directory) then ExistingDirectory := NormalizeDirectory(Directory);
      if ExistingDirectory = '' then begin
        try
          Directory := ExpandConstant('{userdesktop}\MusicOps.lnk');
          if FileExists(Directory) then begin
            Shell := CreateOleObject('WScript.Shell');
            Shortcut := Shell.CreateShortcut(Directory);
            Directory := ExtractFileDir(Shortcut.TargetPath);
            if IsExistingMusicOps(Directory) then ExistingDirectory := NormalizeDirectory(Directory);
          end;
        except
          Log('MusicOps: desktop shortcut lookup unavailable; directory selection remains available.');
        end;
      end;
    end;
  end;
  UpgradeDetected := ExistingDirectory <> '';
  if UpgradeDetected then begin
    if ExistingVersion = '' then GetVersionNumbersString(AddBackslash(ExistingDirectory) + 'MusicOps.exe', ExistingVersion);
    Log('MusicOps install mode=upgrade; detected version=' + ExistingVersion);
  end else Log('MusicOps install mode=fresh');
end;

function GetInstallDirectory(Param: String): String;
begin
  DetectExistingInstallation;
  if UpgradeDetected then Result := ExistingDirectory
  else Result := ExpandConstant('{localappdata}\Programs\MusicOps');
end;

function VersionSuffix: String;
begin
  Result := '';
  if ExistingVersion <> '' then Result := ' ' + ExistingVersion;
end;

function InitializeSetup: Boolean;
var
  OldVersion, NewVersion: Int64;
begin
  DetectExistingInstallation;
  Result := True;
  if UpgradeDetected and (RequestedDirectory <> '') and
    (CompareText(RequestedDirectory, ExistingDirectory) <> 0) then begin
    SuppressibleMsgBox(FmtMessage(CustomMessage('DifferentDirectory'), [ExistingDirectory]), mbError, MB_OK, IDOK);
    Result := False;
  end;
  if Result and UpgradeDetected and StrToVersion(ExistingVersion, OldVersion) and
    StrToVersion('{#AppVersion}', NewVersion) and (ComparePackedVersion(OldVersion, NewVersion) > 0) then begin
    SuppressibleMsgBox(FmtMessage(CustomMessage('NewerVersion'), [ExistingVersion]), mbError, MB_OK, IDOK);
    Result := False;
  end;
end;

procedure InitializeWizard;
begin
  if UpgradeDetected then begin
    WizardForm.DirEdit.Text := ExistingDirectory;
    WizardForm.Caption := CustomMessage('UpgradeTitle');
    WizardForm.WelcomeLabel2.Caption := FmtMessage(CustomMessage('UpgradeWelcome'), [VersionSuffix, ExistingDirectory]);
  end else WizardForm.WelcomeLabel2.Caption := CustomMessage('FreshWelcome');
end;

function ShouldSkipPage(PageID: Integer): Boolean;
begin
  Result := UpgradeDetected and (PageID = wpSelectDir);
end;

function NextButtonClick(CurPageID: Integer): Boolean;
begin
  Result := True;
  { A portable edition selected manually is also an update, even without a receipt. }
  if (CurPageID = wpSelectDir) and IsExistingMusicOps(WizardDirValue) then begin
    ExistingDirectory := NormalizeDirectory(WizardDirValue);
    UpgradeDetected := True;
    Log('MusicOps install mode=upgrade; portable directory selected.');
  end;
end;

procedure CurPageChanged(CurPageID: Integer);
begin
  if UpgradeDetected and (CurPageID = wpReady) then
    WizardForm.NextButton.Caption := CustomMessage('UpdateButton');
end;

function UpdateReadyMemo(Space, NewLine, MemoUserInfoInfo, MemoDirInfo, MemoTypeInfo,
  MemoComponentsInfo, MemoGroupInfo, MemoTasksInfo: String): String;
begin
  if UpgradeDetected then Result := FmtMessage(CustomMessage('UpdateSummary'), [VersionSuffix]) +
    NewLine + Space + ExistingDirectory + NewLine + MemoTasksInfo
  else Result := MemoDirInfo + NewLine + MemoTasksInfo;
end;

function ExecutableAvailableForUpdate(const FileName: String): Boolean;
var
  Handle: TMusicOpsNativeInt;
begin
  Result := True;
  if not FileExists(FileName) then Exit;
  { Opening for write without sharing detects mapped executables on Windows.
    The handle is closed immediately; no content is written and no process is killed. }
  Handle := CreateFileForUpdate(FileName, $C0000000, 0, 0, 3, $80, 0);
  Result := Handle <> -1;
  if Result then CloseFileHandle(Handle);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  Result := '';
  { The legacy worker exe is checked too: an upgrade from a Python install
    must not replace it while it is running. }
  if not ExecutableAvailableForUpdate(ExpandConstant('{app}\MusicOps.exe')) or
    not ExecutableAvailableForUpdate(ExpandConstant('{app}\musicops.exe')) or
    not ExecutableAvailableForUpdate(ExpandConstant('{app}\MusicOps-worker.exe')) then begin
    Log('MusicOps: update blocked by busy or inaccessible executable; no process terminated.');
    Result := CustomMessage('BusyApplication');
  end;
end;
