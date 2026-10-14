; Build with Inno Setup 7: ISCC /DBundleDir=<audited runtime folder> /O<output folder> MusicOps.iss
#ifndef BundleDir
  #define BundleDir "..\dist\MusicOps-Windows"
#endif

[Setup]
AppId=MusicOps.Desktop
AppName=MusicOps
AppVersion=0.2.0
AppPublisher=wsdbb72
AppPublisherURL=https://github.com/wsdbb72/MusicOps
AppSupportURL=https://github.com/wsdbb72/MusicOps/issues
DefaultDirName={localappdata}\Programs\MusicOps
DefaultGroupName=MusicOps
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
WizardStyle=modern
DisableProgramGroupPage=yes
OutputBaseFilename=MusicOps-0.2.0-Setup
Compression=lzma2
SolidCompression=yes
CloseApplications=no
RestartApplications=no
UninstallDisplayIcon={app}\MusicOps.exe
VersionInfoVersion=0.2.0.0
VersionInfoDescription=MusicOps Windows Installer

[Languages]
Name: "chinesesimp"; MessagesFile: "compiler:Languages\ChineseSimplified.isl"
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"

[Dirs]
Name: "{app}\profile"; Flags: uninsneveruninstall

[Files]
Source: "{#BundleDir}\MusicOps.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BundleDir}\MusicOps-worker.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BundleDir}\_internal\*"; DestDir: "{app}\_internal"; Flags: ignoreversion recursesubdirs createallsubdirs
Source: "{#BundleDir}\licenses\*"; DestDir: "{app}\licenses"; Flags: ignoreversion recursesubdirs createallsubdirs
Source: "{#BundleDir}\LICENSE"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BundleDir}\THIRD-PARTY-NOTICES.md"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BundleDir}\启动说明.txt"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\MusicOps"; Filename: "{app}\MusicOps.exe"; Parameters: "--base ""{app}\profile"""; WorkingDir: "{app}"
Name: "{autodesktop}\MusicOps 正式版"; Filename: "{app}\MusicOps.exe"; Parameters: "--base ""{app}\profile"""; WorkingDir: "{app}"; Tasks: desktopicon

[Run]
Filename: "{app}\MusicOps.exe"; Parameters: "--base ""{app}\profile"""; Description: "{cm:LaunchProgram,MusicOps}"; Flags: nowait postinstall skipifsilent
