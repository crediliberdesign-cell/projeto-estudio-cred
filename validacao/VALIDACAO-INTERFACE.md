# Validação da interface — 28/10/2026

Ambiente: macOS, Chrome local em perfil temporário, Playwright 1.x fornecido pelo runtime do Codex. Não houve instalação de componentes do Windows nem inferência na RTX de destino.

## Executado

- `node --check web/app.js`: sem erros de sintaxe.
- `web/.checks/smoke.cjs`: navegador real, API simulada com dados fictícios. Verificou assistente de instalação, cadastro, paleta por controle visual, separação cadastro/projeto, campos de vídeo inicialmente vazios, criação de projeto, chat NDJSON, memória, segundo projeto sem herança de duração/áudio/formato, largura móvel de 390px sem overflow e ausência de exceções JavaScript.
- `web/.checks/integration.cjs`: navegador real com **backend Go real**. Verificou cadastro e persistência de empresa, upload multipart de logo SVG fictício, extração local de paleta com aprovação, conta-gotas sobre o logo, edição de HEX exato persistida, ausência de configurações de vídeo no cadastro de empresa, criação de projeto real, memória salva e recuperada após recarga, separação ao trocar entre duas empresas, novo projeto com campos de vídeo vazios e ausência de exceções JavaScript.
- Inspeção visual das capturas de início, projeto, chat, cadastro de paleta e versão móvel.

A verificação integrada serviu os arquivos estáticos mais recentes pelo interceptador do navegador e alterou somente o campo `installed` na resposta recebida pelo navegador para permitir navegar nas telas diárias. Todas as gravações de empresa/projeto/memória/upload usaram as APIs reais. O banco do servidor continuou `installed:false`; nenhuma compatibilidade Windows é implicada por esse teste.

## Corrigido após encontrar falha real

Empresas sem anexos têm o campo `assets` omitido pelo backend Go. O editor passou a normalizar assets, colors, folders e font_roles ao abrir/atualizar rascunhos. A execução integrada posterior passou.

## Recursos integrados, ainda dependentes de validação da pilha instalada

- Player Remotion incorporado via iframe e mensagem restrita à mesma origem.
- Edição de transcrição por segmentos, preservando estrutura e invalidando timestamps de palavras quando o texto/tempo muda.
- Prévia de vídeos, fotogramas, originais e resultados locais.
- Cancelar e repetir jobs; nova tentativa reutiliza as opções originais.
- Interromper streaming do chat por AbortController.
- Downloads de backup e diagnóstico; diagnóstico usa endpoint sanitizado.

As rotas e os controles não substituem testes reais de download, transcrição CUDA, Remotion, fontes, áudio e exportação no Windows alvo.

As capturas usam somente empresas e materiais fictícios; não são projetos reais do usuário.

## Verificação adicional: Player Remotion real

Executado contra o backend Go recompilado, com o Player já compilado e um job de motion realmente concluído no macOS. Nenhum arquivo estático foi substituído neste teste. A única alteração de resposta foi `installed:true` no navegador para permitir a navegação de desenvolvimento.

- Abriu projeto e job reais pela interface.
- Abriu o modal de prévia e iframe de mesma origem.
- A composição React/Remotion exibiu o texto fictício “Vídeo local”.
- Acionou o botão Play; as capturas de antes/depois apresentaram hashes distintos, confirmando alteração visual durante reprodução.
- Não ocorreram exceções JavaScript nem requisições de rede com falha.
- Fechou o modal normalmente.

Evidência estruturada: `PLAYER-VALIDATION.json`; capturas: `player-frame-0.png`, `player-in-motion.png`, `real-player.png`.

Este primeiro fixture não contém logos, fontes ou áudio. A inspeção descobriu uma divergência de rota: os assets de motion são criados em `motion-assets`, enquanto o servidor inicialmente procurava `assets`. Reportado ao responsável pelo backend para correção antes do pacote final. O teste sem assets não cobre esse caminho.

## Verificação final com logo e transferência de empresas

Após corrigir o backend para servir `motion-assets`, foi criado via API um logo SVG fictício, um novo projeto e um novo job real de motion. O Player compilado exibiu o logo local e a composição em movimento. A requisição do logo retornou HTTP 200; não houve falhas JavaScript ou de rede. `PLAYER-VALIDATION.json` e `real-player.png` foram atualizados com essa evidência. O teste anterior sem assets foi complementado; a rota com logo foi efetivamente exercitada.

Também executado no backend real, sem substituir arquivos estáticos:

- Exportação ZIP: arquivos de marca exigem seleção explícita; mídia de projetos permanece desligada separadamente.
- Prévia local do ZIP com limite de leitura/descompressão do cadastro.
- Importação do pacote como nova empresa, com bytes do logo e SHA-256 iguais ao original.
- Cadastro original preservado; acesso a pastas, compartilhamento e configurações de vídeo não herdados.
- Projeto criado no escopo `files`, somente arquivos importados.
- Arquivo principal importado e prévia de vídeo carregada: duração detectada de 1,966667 s.
- Atualização deliberada da identidade no projeto, com confirmação mostrando a versão; versão anterior mantida no histórico.
- Briefing, proporção, duração, áudio, escopo e fonte preservados durante a atualização da identidade.
- Nenhuma exceção JavaScript.

Resultados: `FINAL-INTEGRATION.json`; captura da prévia de importação: `real-company-import.png`.

Uma correção de apresentação substituiu o nome interno de armazenamento pelo nome original do arquivo de vídeo no rascunho e no projeto. A sintaxe da versão final passou na verificação Node. A compilação final deve incorporar `app.js` após essa correção.

Esses testes continuam sendo testes macOS/Chrome da aplicação e das APIs reais. O indicador `installed` foi alterado somente na resposta recebida pelo navegador para navegar no ambiente de desenvolvimento. Eles não demonstram a instalação Windows nem o desempenho da RTX 5060 Ti.
