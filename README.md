# jev-cli

Transforma uma frase em linguagem natural em um comando shell — **sem que nenhum
modelo de linguagem escreva o comando**.

```console
$ jev "liste todos os arquivos desse diretório"
$ ls .
  comando               list_directory · confiança 0.93
  severidade            0.05
  target_path           . (confiança 0.97)
  ls_hidden             omitido (p=0.05)
  ls_long               omitido (p=0.05)
  ls_sort_time          omitido (p=0.05)
  ls_sort_size          omitido (p=0.05)
  ls_recursive          omitido (p=0.05)
dry-run: nada foi executado. Use -x para executar.

$ jev -x "liste todos os arquivos desse diretório"
cmd
go.mod
internal
```

O jev é um modelo **System One** da [TypeSafe AI](https://docs.typesafe.ai): ele
não gera texto. Ele responde perguntas tipadas — `Choice`, `Score`, `Noul` —
sobre um **catálogo fechado** de comandos, e o `jev-cli` monta o `argv` a partir
das respostas.

## Por que assim

Um CLI que pede a um LLM "me devolve o comando shell" tem dois problemas: o
comando pode ser qualquer coisa, e o texto gerado precisa ser interpretado. Aqui
as duas coisas somem por construção:

- **Nada é gerado.** Toda string que chega à linha de comando ou foi escrita por
  este repositório (as flags, os programas) ou foi confirmada existir por código
  (caminhos, padrões, termos). Não existe caminho do modelo para um comando
  arbitrário: a resposta dele é sempre uma chave de um conjunto que nós
  definimos.
- **Nada passa por um shell.** O comando é um `[]string` executado com
  `exec.CommandContext`. Não há `sh -c`, então aspas, `;`, `|`, `$()` e crases
  não têm como virar um segundo comando.
- **A decisão de executar é do código.** Confiança e probabilidades calibradas
  decidem entre agir, perguntar e recusar — e o padrão é *não executar*.

## Instalação

```console
$ go install github.com/harlleyoliveira/jev-cli/cmd/jev@latest
```

Sem dependências externas: só a biblioteca padrão do Go.

A API key vem de `TYPESAFE_API_KEY`, do arquivo de config, ou de
`--api-key-file`. Nunca de uma flag comum — argumentos aparecem no `ps`.

```console
$ export TYPESAFE_API_KEY=...
$ jev "onde eu estou"
```

Ou no arquivo (veja [Configuração](#configuração)):

```json
{ "api_key": "ts_..." }
```

## Uso

```
jev [opções] "frase em linguagem natural"
```

| Opção | Efeito |
| --- | --- |
| *(padrão)* | dry-run: mostra o comando resolvido e as decisões, e para |
| `-x`, `--execute` | executa o comando resolvido |
| `--allow-write` | permite comandos que não sejam de leitura |
| `-j`, `--json` | resultado em JSON; o comando é capturado em vez de herdado |
| `--path VALOR` | usa este caminho, sem perguntar ao modelo |
| `--pattern VALOR` | usa este padrão de nome de arquivo |
| `--term VALOR` | usa este texto de busca |
| `-v`, `--explain` | mostra o pedido, a resposta bruta, notas e uso de tokens |
| `--commands` | lista o catálogo fechado e o que está disponível aqui |
| `-m`, `--model` | modelo (padrão `jev-latest`) |
| `--base-url` | host da API |
| `--max-entries` | máximo de entradas do diretório no state |
| `--min-confidence` | confiança mínima para agir (padrão `0.5`) |
| `--timeout` | timeout da chamada, em segundos |
| `--no-color` | sem cores |

Com `-x`, o código de saída do comando executado é propagado. Sem `-x`, os
códigos são: `0` sucesso, `1` erro, `2` não resolvido (ambíguo ou fora do
catálogo), `3` bloqueado por guardrail, `4` sem API key.

## Como funciona

```
frase
  │
  ├─ CÓDIGO  sonda o ambiente (env.Probe)
  │            cwd, listagem do diretório (contada e filtrada aqui),
  │            repo git, binários no PATH, e os candidatos extraídos
  │            da frase: caminhos que existem, globs, termos
  │
  ├─ 1 REQUEST  POST /v1/systemone
  │            state  = frase + ambiente filtrado
  │            questions =
  │               intent                    Choice sobre o catálogo
  │               target_path               Choice sobre caminhos reais
  │               name_pattern / search_terms / flags   (especulativas)
  │               <slot>?                   Noul "o usuário disse algo sobre isso?"
  │               guardrail.injection       Noul
  │               guardrail.destructive_request  Noul
  │               guardrail.intent_clear    Noul
  │               guardrail.severity        Score
  │
  ├─ CÓDIGO  lê só as respostas do comando vencedor (catalog.Assemble)
  │            aplica os gates, monta o argv
  │
  └─ CÓDIGO  dry-run por padrão; executa com -x
```

Todas as perguntas vão em **uma única chamada**: o modelo as avalia em paralelo,
então uma pergunta especulativa sobre um comando que perdeu custa tokens, não
latência. É o padrão de *speculative fan-out* da documentação da TypeSafe.

### Quatro padrões da doc que este CLI usa

1. **Function calling.** Cada argumento de conjunto fechado vira uma `Choice`
   cujas chaves são exatamente os valores aceitos. Nada precisa mapear um rótulo
   de volta para um argumento: a resposta já *é* o token.
2. **`stated` (`<slot>?`).** Antes de usar um argumento opcional — uma flag ou
   um valor —, um `Noul` pergunta se o usuário **disse algo** sobre ele. Se não
   disse, o default declarado vale. É isso que impede uma pergunta respondida no
   silêncio de virar uma decisão: com o modelo real, "liste todos os arquivos
   desse diretório" responde a flag de arquivos ocultos em **p=0.52** — logo
   acima da linha de 0.5 — e o comando saía `ls -a .`. A mesma frase tem o gate
   em p=0.18, então a flag não entra. Quando o pedido realmente fala de arquivos
   ocultos, o gate sobe para 0.93 e o `-a` aparece.
3. **Pre-parsed value extraction.** Caminhos, padrões e termos de busca são
   strings abertas, e o jev não as produz. O código faz *over-find* com regex e
   `stat`, e o modelo apenas **seleciona** entre os candidatos. Tudo que volta é
   verbatim: não dá para inventar ou trocar um dígito.
4. **Confidence-gated routing + guardrails.** Os gates usam os números da doc
   (`0.5` para intenção ambígua, `0.35` para revisar, `0.70` para agir sobre um
   perigo, `2.0` de severidade) e são configuráveis. As perguntas de guardrail
   rodam na mesma chamada, então não custam latência.

### Contagem e aritmética ficam no código

O jev não conta de forma confiável e níveis de `Score` têm calibração numérica
fraca, então `entry_count`, profundidade de busca e ordenação são computados aqui
— nunca perguntados ao modelo.

## O catálogo

Tudo que este CLI pode executar, e nada mais:

| Comando | O que faz |
| --- | --- |
| `list_directory` | lista o conteúdo de um diretório |
| `find_files` | procura arquivos por nome, com profundidade limitada |
| `search_text` | procura texto dentro de arquivos (`rg`, ou `grep`), com filtro de nome opcional |
| `show_file` | imprime um arquivo, inteiro ou uma ponta |
| `count_lines` | conta linhas |
| `disk_usage` | tamanho de um caminho |
| `file_info` | que tipo de arquivo é |
| `report_working_directory` | imprime o diretório atual |
| `git_status`, `git_log`, `git_diff` | estado, histórico e diff do repositório |

Todos são **de leitura**. `--commands` mostra o catálogo e marca o que está
indisponível no ambiente atual (por exemplo, os comandos `git_*` fora de um
repositório).

Adicionar um comando é adicionar uma entrada em
[`internal/catalog/entries.go`](internal/catalog/entries.go): um template de
`argv` com placeholders, os slots que decidem cada placeholder, e as descrições
contrastivas (*o que é* / *para que não é*) que mantêm comandos vizinhos
distinguíveis.

## Calibração com o modelo real

Os thresholds acima são pontos de partida. Estes são os números que o `jev` de
verdade devolveu para este projeto, e o que eles mudaram:

| Frase | Antes | Depois |
| --- | --- | --- |
| `liste todos os arquivos desse diretório` | `ls -a .` (flag em 0.52, falso positivo) | `ls .` (gate em 0.18) |
| `liste tudo, incluindo os arquivos ocultos` | `ls -a .` | `ls -a .` (p=0.98, correto) |
| `procure por TODO nos arquivos go` | `rg -e TODO .` (ignorava "arquivos go") | `rg -g '*.go' -e TODO .` |
| `o que mudou` | `ask` sem saída útil | `ask` + sugestão `git status` + candidatos |

Os gates de confiança continuam sendo o lugar mais provável de precisar de
ajuste. `o que mudou` fica em `ask` porque o modelo divide entre `git_status` e
`git_diff` (0.40) — o que é uma ambiguidade real, não um erro. Para quem preferir
que ele aja nesse caso, `min_confidence` no config resolve; o comando continua
sendo de leitura.

## Limites conhecidos

- **Não encadeia comandos.** Sem pipe: um `argv`, um programa. Frases que pedem
  duas coisas ("liste e depois ordene por tamanho") caem em `ask` ou
  `unsupported`. Um pipeline interno é o próximo passo natural.
- **Não escreve nada.** Pedidos de alteração são recusados com essa explicação,
  em vez de atendidos com algo parecido. Escrever exigiria uma taxonomia de risco
  de verdade — as flags já existem (`ReadOnly`, `--allow-write`), mas nenhuma
  entrada é de escrita ainda.
- **Vocabulário fechado é fechado.** O que não está no catálogo não é feito. A
  saída honesta é `unsupported` com os candidatos mais prováveis e suas
  probabilidades.
- **255 opções por `Choice`.** A listagem do diretório é limitada por
  `max_entries`; um comando cujos candidatos não couberem (ou não existirem)
  simplesmente não fica disponível naquela invocação, em vez de virar um 422.
- **`state` não é hostil para o modelo.** Texto livre — inclusive nomes de
  arquivo — pode influenciar respostas. Por isso: listagem filtrada e limitada,
  guardrail de injeção, e um allowlist de programas como última linha de defesa.

## Configuração

`~/.config/jev/config.json` (ou `$XDG_CONFIG_HOME/jev/config.json`, ou
`$JEV_CONFIG`). Chaves opcionais:

```json
{
  "api_key": "",
  "model": "jev-latest",
  "base_url": "https://api.typesafe.ai",
  "max_entries": 120,
  "min_confidence": 0.5,
  "clarity_threshold": 0.35,
  "destructive_threshold": 0.35,
  "injection_threshold": 0.7,
  "severity_threshold": 2.0,
  "timeout_seconds": 30,
  "no_color": false,
  "allow_write": false
}
```

Variáveis de ambiente: `TYPESAFE_API_KEY`, `JEV_MODEL`, `JEV_BASE_URL`,
`JEV_CONFIG`, `NO_COLOR`. Se o arquivo contiver uma API key e for legível por
outros usuários, o CLI avisa.

## Desenvolvimento

```console
$ go test ./...
$ go vet ./...
```

A suíte cobre o cliente HTTP (incluindo retry em `429`/`529` e a recusa em
retentar `401`/`422`), a montagem do `argv` (grupos exclusivos, dependências
entre flags, defaults, valores forçados), a extração de candidatos, os vereditos
de guardrail, e um teste **end-to-end** que roda o binário contra um servidor
TypeSafe falso — inclusive executando de verdade o `ls` resolvido.

## Licença

MIT.
