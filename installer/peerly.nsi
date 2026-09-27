; Installer for peerly.exe. Installs for the current user only, so it never
; asks for administrator rights, and puts peerly in the Start menu so Windows
; search finds it.
;
;   makensis -DVERSION=0.2.0 installer\peerly.nsi      (after make windows)
;
; Settings, the group login and the local save backups live in %AppData%\peerly
; and are never touched by the installer or the uninstaller.
;
; Paths use backslashes: makensis on Windows does not accept ../ in File.

Unicode true
SetCompressor /SOLID lzma

!ifndef VERSION
  !define VERSION "0.0.0"
!endif
!ifndef DIST
  !define DIST "..\dist"
!endif
!define UNINSTALL_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\peerly"

Name "peerly"
OutFile "${DIST}\peerly-setup.exe"
InstallDir "$LOCALAPPDATA\Programs\peerly"
InstallDirRegKey HKCU "${UNINSTALL_KEY}" "InstallLocation"
RequestExecutionLevel user
BrandingText "peerly ${VERSION}"

VIProductVersion "${VERSION}.0"
VIAddVersionKey "ProductName" "peerly"
VIAddVersionKey "FileDescription" "peerly setup"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "LegalCopyright" "MIT License"

!include "MUI2.nsh"
!include "FileFunc.nsh"

!define MUI_ICON "..\client\app\winres\icon.ico"
!define MUI_UNICON "..\client\app\winres\icon.ico"
!define MUI_FINISHPAGE_RUN "$INSTDIR\peerly.exe"
!define MUI_FINISHPAGE_RUN_TEXT "Open peerly"

!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

; A running peerly.exe cannot be replaced or removed. It is never closed from
; here: the person may be hosting, and closing then would keep their progress
; from the group.
!macro WaitUntilClosed
  retry:
    ClearErrors
    Delete "$INSTDIR\peerly.exe"
    IfErrors 0 closed
      MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION \
        "peerly is open. If you are hosting, close the game and wait until the world is free. Then close peerly and press Retry." \
        /SD IDCANCEL IDRETRY retry
      Abort
  closed:
!macroend

Section
  SetShellVarContext current
  !insertmacro WaitUntilClosed
  SetOutPath "$INSTDIR"
  File "${DIST}\peerly.exe"
  WriteUninstaller "$INSTDIR\uninstall.exe"
  CreateShortcut "$SMPROGRAMS\peerly.lnk" "$INSTDIR\peerly.exe" "" "$INSTDIR\peerly.exe" 0

  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayName" "peerly"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "Publisher" "peerly"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "URLInfoAbout" "https://github.com/ssaruul/peerly"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayIcon" "$INSTDIR\peerly.exe"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKCU "${UNINSTALL_KEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
  WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoRepair" 1
  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  WriteRegDWORD HKCU "${UNINSTALL_KEY}" "EstimatedSize" $0
SectionEnd

Section "Uninstall"
  SetShellVarContext current
  !insertmacro WaitUntilClosed
  Delete "$SMPROGRAMS\peerly.lnk"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"
  DeleteRegKey HKCU "${UNINSTALL_KEY}"
SectionEnd
