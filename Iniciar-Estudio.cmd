@echo off
chcp 65001 >nul
title Estudio Local - Terminal (Servidor Ativo)
color 0B

echo ==================================================================
echo              ESTÚDIO LOCAL — MODO PROGRAMA / TERMINAL
echo ==================================================================
echo.
echo  Iniciando o servidor local na porta fixa 8765...
echo  O navegador sera aberto automaticamente com o seu painel.
echo.
echo  [ATENCAO]
echo  - Mantenha esta janela de terminal aberta enquanto estiver usando.
echo  - O terminal mantem o servidor ligado e mostra o progresso.
echo  - Para encerrar o programa, pressione Ctrl+C ou feche esta janela.
echo.
echo ==================================================================
echo.

cd /d "%~dp0"
"%~dp0EstudioLocal-Instalar.exe" --port 8765

if %ERRORLEVEL% NEQ 0 (
    echo.
    echo [ERRO] O servidor foi encerrado com codigo de erro %ERRORLEVEL%.
    echo.
    pause
)
