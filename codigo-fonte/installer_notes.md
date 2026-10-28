# Instalador Windows — integração e evidências

Implementado em `installer.go`, sem bibliotecas Go externas. Distribuição alvo: Windows 11 x64. O aplicativo funciona como usuário comum; o pré-requisito Microsoft Visual C++ pode pedir consentimento UAC/TI para instalar ou atualizar o runtime do sistema quando necessário. O pacote não modifica o Ollama/WebUI existente, PATH global, política permanente de PowerShell, antivírus ou serviços da empresa.

## Contrato

- `RunInstall(ctx, root, emit) (RuntimeConfig,error)` pressupõe que o servidor já copiou os assets embutidos para `root/assets/media` e `root/assets/remotion`.
- `InstallEvent` usa `stage`, `progress` entre 0 e 1 e `message`; as etapas sem contagem de bytes são marcos de progresso, não uma estimativa de tempo.
- `RuntimeConfig` contém caminhos locais absolutos, modelos e URL privada. Campos JSON em snake_case. `WhisperModelPath` aponta para o snapshot local completo, `OllamaExecutable` para o binário portátil.
- `StartOllama(ctx,*cfg)` escolhe porta livre em 127.0.0.1 e atualiza `cfg.OllamaURL`. Salvar cfg depois de iniciar. A instância iniciada para instalar é encerrada antes de `RunInstall` retornar; o servidor deve iniciar a instância de uso normal.
- `StopOllama(cmd)` encerra somente a árvore do processo criado pelo app. Deve ser chamado ao fechar o app. Nenhum processo é identificado apenas pelo número da porta.
- `InspectInstallTarget` retorna versão/build Windows, RAM, placa/driver e bytes livres na unidade escolhida. `GetDownloadManifest` fornece tamanho estimado de cada download para a interface.

## Downloads e configuração

Manifesto embutido `downloads.json`, revisado em 28/10/2026. Metadados e hashes consultados diretamente nos serviços oficiais:

- Ollama 0.40.12, ZIP portátil Windows x64 (~1,60 GB), release GitHub Ollama.
- uv 0.13.6, ZIP Windows x64 (~15 MB), release GitHub Astral.
- Node 22.23.4 LTS, ZIP Windows x64 (~34 MB estimados), SHASUMS256.txt de nodejs.org.
- FFmpeg 9.0.2 essentials, ZIP Gyan (~115 MB), build externo indicado pelo FFmpeg, inclui FFprobe e libass. Esta build é GPLv3; preservar os avisos e termos de redistribuição.
- Microsoft Visual C++ x64 14.51.36247.0, EXE oficial de 18,7 MB. SHA256 calculado diretamente sobre o arquivo da Microsoft e fixado; o Windows também verifica a assinatura Authenticode Microsoft antes da execução. Versões posteriores já instaladas são preservadas. Bloqueio/cancelamento do UAC informa que é necessário solicitar autorização ao TI, sem elevar o aplicativo inteiro.
- Python 3.12.12 gerenciado pelo uv fixado. O mapeamento do build standalone é fornecido por essa versão fixada do uv; não é um instalador do python.org.
- qwen3.5:4b (~3,32 GB) e qwen3.5:9b (~6,55 GB), embeddinggemma (~622 MB). API Ollama verifica blobs; após pull o instalador exige o digest de manifesto registrado. Se o proprietário do registro alterar o tag, o instalador exige atualização do manifesto em vez de usar silenciosamente outros pesos.
- Whisper large-v3-turbo multilíngue, repo `dropbox-dash/faster-whisper-large-v3-turbo`, revisão `0a363e9161cbc7ed1431c9597a8ceaf0c4f78fcf`. Cinco arquivos com SHA256 individual, incluindo pesos ~1,62 GB. Não usa modelos `.en`.
- Dependências do worker via venv privado. O `requirements.lock` completo contém 33 pacotes e hashes para Windows/Python 3.12; instalação usa `--require-hashes --only-binary :all:` e registra freeze. A ausência do lock interrompe a instalação com uma mensagem de pacote incompleto.
- Remotion via `npm ci` com lock, build do Player e `remotion browser ensure`, preparando o navegador local antes do uso offline.
- Bibliotecas CUDA NVIDIA privadas no venv, configuradas pelo worker. Não se infere que CUDA do Ollama serve para CTranslate2. O worker diagnostica a disponibilidade e declara fallback para CPU.

35 GiB livres inicialmente, reduzidos pelos arquivos de instalação já baixados ao retomar, mantendo reserva mínima de 12 GiB. Isso é espaço para instalação e cache; projetos de vídeo precisam de espaço adicional. Os downloads são feitos apenas na instalação. Conversa e renderização local não exigem rede após bootstrap completo; pesquisa web continua exigindo rede.

Downloads grandes preservam `.part`, validam Content-Range quando retomados e verificam SHA256 antes de extrair/executar. Se o servidor ignora Range, o arquivo parcial é reiniciado. Checksum incorreto rejeita/remove o download. ZIPs rejeitam traversal, caminhos absolutos, drive letters, links e extrações acima de 16 GiB. Pastas finais de runtimes não identificadas pelo instalador nunca são sobrescritas.

Ollama usa modelos dentro da pasta escolhida, um modelo por vez, paralelismo 1, cache KV q8_0, contexto 8192 e cloud desabilitado. As opções por requisição devem ainda controlar contexto, `think`, streaming e tempo de keepalive conforme o tipo de tarefa. Essas escolhas são limites de recursos, não benchmark de velocidade.

## Verificações executadas aqui

A suíte Go passou no macOS arm64, inclusive com detector de condições de corrida; contempla os testes de instalador e os testes de integração adicionados em integration_test.go. Cobertura inclui: manifesto, download com retomada, servidor ignorando Range, checksum incorreto, Content-Range incorreto, ZIP com traversal/link, extração normal, cancelamento e digest de modelo divergente. Servidor de teste usa apenas loopback; a sandbox exigiu permissão para bind.

Não foram executados neste ambiente: instalação real no Windows, autenticação/políticas da rede corporativa, download integral de todos os modelos, carregamento em RTX 5060 Ti, tempos de resposta/transcrição ou NVENC nessa placa. Não declarar compatibilidade/performance de hardware como verificada.

No computador alvo o bootstrap executa, antes de informar conclusão:

1. Verificação dos executáveis, importações Python e filtro subtitles/libass.
2. Transcrição real offline de `media/sample-pt.wav`, amostra CC0 em português; exige texto e registra dispositivo/compute_type/avisos/tempo. É uma única fala curta, sem alegação de benchmark de qualidade. GPU só é marcada verificada para esta amostra quando o worker efetivamente retorna `device=cuda`.
3. Duas respostas curtas do modelo rápido 4B, uma após descarregar e outra com o modelo carregado. Streaming, think=false, contexto2048 e limite48tokens. Registra TTFT, wall time, load/prompt/eval durations/counts retornados pelo Ollama e /api/ps para residência GPU/CPU. Não mede tarefas longas nem o modelo9B.
4. Um motion real de2segundos,16:9, resolução curta320px, marca neutra e sem áudio. Exige MP4 local, duração correta, faixa de vídeo, ausência de áudio e decodificação FFmpeg sem falha.
5. Diagnóstico do worker com tentativa curta de NVENC. Falha de NVENC não bloqueia uso de CPU; é registrada como indisponibilidade/fallback.

Evidência em `logs/readiness.json`, `logs/readiness-asr`, `logs/readiness-motion` e `logs/readiness-diagnose`. Esses testes serão executados no computador de destino; não foram executados no Windows durante a criação do pacote.

## Limites conhecidos

- EXE sem assinatura comercial pode disparar SmartScreen/controle corporativo. Não orientar desligar a proteção; o administrador da empresa decide a autorização do aplicativo.
- Fontes/logos/briefings devem permanecer nas pastas de dados/projetos e não nos diretórios de runtime.
- Chromium, npm e Python usam downloads transitivos definidos pelos respectivos lockfiles e ferramentas; o manifesto de arquivos principais não substitui seus mecanismos de integridade.
- O instalador interrompe e informa erros; não mascara falta de rede, pacote alterado, bloqueio do Windows ou modelo indisponível como sucesso.
