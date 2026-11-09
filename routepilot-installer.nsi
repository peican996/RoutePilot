; RoutePilot NSIS 安装脚本
; 用法: makensis routepilot-installer.nsi
; 需要先运行 python package.py 生成 bin/RoutePilot.exe

!include "MUI2.nsh"
!include "FileFunc.nsh"

; ---- 基本信息 ----
; 版本号可由 package.py 通过 /DAPP_VERSION=x.y.z 覆盖，避免两处硬编码不同步
!ifndef APP_VERSION
  !define APP_VERSION "1.0.0"
!endif
!define APP_NAME "RoutePilot"
!define APP_PUBLISHER "RoutePilot Project"
!define APP_EXE "RoutePilot.exe"
!define UNINST_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APP_NAME}"

Name "${APP_NAME} ${APP_VERSION}"
OutFile "RoutePilot-${APP_VERSION}-setup.exe"
InstallDir "$PROGRAMFILES64\${APP_NAME}"
InstallDirRegKey HKLM "Software\${APP_NAME}" "InstallDir"
RequestExecutionLevel admin

; ---- 现代 UI ----
!define MUI_ABORTWARNING
!define MUI_ICON "build\appicon.ico"
!define MUI_UNICON "build\appicon.ico"

!insertmacro MUI_PAGE_WELCOME
; 仓库里的许可证文件是 LICENSE（无扩展名），这里必须写实际路径，
; 否则 makensis 会因找不到 LICENSE.txt 直接失败。
!insertmacro MUI_PAGE_LICENSE "LICENSE"
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "SimpChinese"
!insertmacro MUI_LANGUAGE "English"

; ---- 安装 ----
Section "Install"
    SetOutPath "$INSTDIR"

    ; 主程序
    File "bin\${APP_EXE}"

    ; 写注册表（卸载入口）
    WriteRegStr HKLM "${UNINST_KEY}" "DisplayName" "${APP_NAME}"
    WriteRegStr HKLM "${UNINST_KEY}" "DisplayVersion" "${APP_VERSION}"
    WriteRegStr HKLM "${UNINST_KEY}" "Publisher" "${APP_PUBLISHER}"
    WriteRegStr HKLM "${UNINST_KEY}" "DisplayIcon" "$INSTDIR\${APP_EXE}"
    WriteRegStr HKLM "${UNINST_KEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
    WriteRegStr HKLM "${UNINST_KEY}" "InstallLocation" "$INSTDIR"
    ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
    IntFmt $0 "0x%08X" $0
    WriteRegDWORD HKLM "${UNINST_KEY}" "EstimatedSize" "$0"

    ; 卸载器
    WriteUninstaller "$INSTDIR\uninstall.exe"

    ; 开始菜单快捷方式
    CreateDirectory "$SMPROGRAMS\${APP_NAME}"
    CreateShortCut "$SMPROGRAMS\${APP_NAME}\${APP_NAME}.lnk" "$INSTDIR\${APP_EXE}"
    CreateShortCut "$SMPROGRAMS\${APP_NAME}\卸载 ${APP_NAME}.lnk" "$INSTDIR\uninstall.exe"

    ; 桌面快捷方式
    CreateShortCut "$DESKTOP\${APP_NAME}.lnk" "$INSTDIR\${APP_EXE}"
SectionEnd

; ---- 卸载 ----
Section "Uninstall"
    Delete "$INSTDIR\${APP_EXE}"
    Delete "$INSTDIR\uninstall.exe"
    RMDir "$INSTDIR"

    Delete "$SMPROGRAMS\${APP_NAME}\${APP_NAME}.lnk"
    Delete "$SMPROGRAMS\${APP_NAME}\卸载 ${APP_NAME}.lnk"
    RMDir "$SMPROGRAMS\${APP_NAME}"
    Delete "$DESKTOP\${APP_NAME}.lnk"

    DeleteRegKey HKLM "${UNINST_KEY}"
    DeleteRegKey HKLM "Software\${APP_NAME}"

    ; 配置目录保留（%APPDATA%\RoutePilot），用户可能想保留订阅
    ; IDNO 要跳过下面 3 条 RMDir，点「否」时一个都不删
    MessageBox MB_YESNO "是否同时删除配置数据（订阅、规则、流量历史）？" IDNO +3
    RMDir /r "$APPDATA\RoutePilot"
    ; 兼容旧版本目录名
    RMDir /r "$APPDATA\NexusLink"
    RMDir /r "$APPDATA\ProxDeck"
SectionEnd
