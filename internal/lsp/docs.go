package lsp

// Russian documentation for keywords, builtins and annotations (hover and completion).

type kwDoc struct {
	doc     string
	snippet string // optional snippet for completion
	label   string // snippet label
}

var keywordDocs = map[string]kwDoc{
	"package":     {doc: "Объявление пакета. Все файлы `.gocpy` одной директории — один пакет.", snippet: "package ${1:main}"},
	"import":      {doc: "Импорт пакетов: `std/...` — стандартная библиотека, путь модуля из `gocpy.yml` — пакеты Gocpy, раздел `go:` — пакеты Go. Конфликты имён запрещены.", snippet: "import (\n\t\"${1:std/fmt}\"\n)", label: "import (…)"},
	"func":        {doc: "Объявление функции, метода (`func (r T) M()`), оператора (`func (a T) +(b T) T`) или функционального литерала.", snippet: "func ${1:name}(${2}) ${3}{\n\t$0\n}", label: "func name() {…}"},
	"type":        {doc: "Объявление типа: структуры, интерфейса, псевдонима или дженерика `type List[T any] struct {…}`.", snippet: "type ${1:Name} struct {\n\t$0\n}", label: "type Name struct {…}"},
	"var":         {doc: "Объявление переменной. Затенение запрещено, неиспользуемые переменные — ошибка.", snippet: "var ${1:name} ${2:int}"},
	"const":       {doc: "Объявление константы (вычисляется при компиляции, поддерживается `iota`).", snippet: "const ${1:Name} = ${2:0}"},
	"struct":      {doc: "Структурный тип: набор именованных полей."},
	"interface":   {doc: "Интерфейс: набор методов или ограничение дженерика (`~int | float64`, `comparable`)."},
	"map":         {doc: "Словарь `map[K]V`. Порядок обхода — порядок вставки."},
	"chan":        {doc: "Канал `chan T` для обмена данными между потоками (`go`)."},
	"if":          {doc: "Условный оператор. Допускает инициализатор: `if x := f(); x > 0 {…}`.", snippet: "if ${1:cond} {\n\t$0\n}", label: "if cond {…}"},
	"else":        {doc: "Ветка `else` условного оператора.", snippet: "else {\n\t$0\n}", label: "else {…}"},
	"for":         {doc: "Цикл: `for {…}`, `for cond {…}`, `for i := 0; i < n; i++ {…}`, `for x in xs {…}`, `for i, x in xs {…}`.", snippet: "for ${1:x} in ${2:xs} {\n\t$0\n}", label: "for x in xs {…}"},
	"in":          {doc: "Обход коллекции в `for … in` и генераторах списков `[x * 2 for x in xs if x > 0]`."},
	"switch":      {doc: "Множественный выбор; `case` не проваливается без `fallthrough`. Есть `switch x := v.(type)`.", snippet: "switch ${1:x} {\ncase ${2:value}:\n\t$0\ndefault:\n}", label: "switch x {…}"},
	"case":        {doc: "Ветка `switch` или `select`."},
	"default":     {doc: "Ветка по умолчанию в `switch` или `select`."},
	"break":       {doc: "Выход из цикла, `switch` или `select`."},
	"continue":    {doc: "Переход к следующей итерации цикла."},
	"fallthrough": {doc: "Переход в следующую ветку `case` без проверки условия."},
	"return":      {doc: "Возврат из функции."},
	"go":          {doc: "Запуск функции в отдельном потоке (пул pthread).", snippet: "go func() {\n\t$0\n}()", label: "go func() {…}()"},
	"defer":       {doc: "Отложенный вызов при выходе из функции (в обратном порядке).", snippet: "defer ${1:f}()"},
	"select":      {doc: "Ожидание операций с несколькими каналами.", snippet: "select {\ncase ${1:v} := <-${2:ch}:\n\t$0\n}", label: "select {…}"},
	"try":         {doc: "Блок с перехватом исключений. Вызовы функций с `@throws` допустимы только внутри `try` или в функциях с `@throws`.", snippet: "try {\n\t$1\n} catch ${2:e} {\n\t$0\n}", label: "try {…} catch e {…}"},
	"catch":       {doc: "Перехват исключения: `catch e {…}` (любое, тип `Exception`) или `catch e: MyError {…}` (по типу).", snippet: "catch ${1:e} {\n\t$0\n}", label: "catch e {…}"},
	"finally":     {doc: "Блок, выполняемый всегда после `try`/`catch`.", snippet: "finally {\n\t$0\n}", label: "finally {…}"},
	"throw":       {doc: "Выброс исключения. Функция должна быть помечена `@throws` или вызов должен быть внутри `try`.", snippet: "throw ${1:err}"},
	"unsafe":      {doc: "Небезопасный блок: доступны `malloc`, `calloc`, `realloc`, `free`, `memcpy`, `memset`, `slice`, арифметика указателей и приведения `(*T)(p)`.", snippet: "unsafe {\n\t$0\n}", label: "unsafe {…}"},
}

var builtinDocs = map[string]string{
	"len":     "`len(x) int` — длина строки, среза, массива, словаря или канала.",
	"cap":     "`cap(x) int` — ёмкость среза, массива или канала.",
	"append":  "`append(s []T, xs ...T) []T` — добавляет элементы в срез.",
	"make":    "`make(T, args...)` — создаёт срез, словарь или канал.",
	"new":     "`new(T) *T` — выделяет память под значение типа T (под управлением GC).",
	"sizeof":  "`sizeof(T) uintptr` — размер типа в байтах.",
	"close":   "`close(ch)` — закрывает канал.",
	"delete":  "`delete(m, key)` — удаляет ключ из словаря.",
	"malloc":  "`malloc(n uintptr) *byte` — выделяет память вне GC (только в `unsafe`). Освобождается через `free`.",
	"calloc":  "`calloc(n, size uintptr) *byte` — выделяет обнулённую память вне GC (только в `unsafe`).",
	"realloc": "`realloc(p *T, n uintptr) *T` — изменяет размер блока (только в `unsafe`).",
	"free":    "`free(p)` — освобождает память, выделенную `malloc`/`calloc`/`realloc` (только в `unsafe`).",
	"memcpy":  "`memcpy(dst, src, n)` — копирует n байт (только в `unsafe`).",
	"memset":  "`memset(p, value, n)` — заполняет n байт (только в `unsafe`).",
	"slice":   "`slice(p *T, n int) []T` — срез поверх сырой памяти (только в `unsafe`).",

	"bool": "Логический тип: `true` или `false`.", "string": "Неизменяемая строка UTF-8.",
	"int": "Целое со знаком, 64 бита.", "int8": "Целое со знаком, 8 бит.", "int16": "Целое со знаком, 16 бит.",
	"int32": "Целое со знаком, 32 бита.", "int64": "Целое со знаком, 64 бита.",
	"uint": "Целое без знака, 64 бита.", "uint8": "Целое без знака, 8 бит.", "uint16": "Целое без знака, 16 бит.",
	"uint32": "Целое без знака, 32 бита.", "uint64": "Целое без знака, 64 бита.", "uintptr": "Целое размера указателя.",
	"float32": "Число с плавающей точкой, 32 бита.", "float64": "Число с плавающей точкой, 64 бита.",
	"byte": "Псевдоним `uint8`.", "rune": "Псевдоним `int32` — символ Unicode.",
	"any":        "Пустой интерфейс: значение любого типа.",
	"error":      "Интерфейс ошибки: `interface { Error() string }`.",
	"Exception":  "Тип перехваченного исключения в `catch e {…}` (аналог `Exception` в Java).",
	"comparable": "Ограничение дженерика: типы, поддерживающие `==` и `!=`.",
	"thread":     "Дескриптор потока.",
	"true":       "Логическая константа «истина».", "false": "Логическая константа «ложь».",
	"nil":  "Нулевое значение указателей, срезов, словарей, каналов, интерфейсов и функций.",
	"iota": "Счётчик в группе `const`: 0, 1, 2…",
}

type annotDoc struct {
	doc     string
	snippet string
}

var annotDocs = map[string]annotDoc{
	"throws":     {doc: "Функция может выбрасывать исключения. Её вызов допустим только в `try` или в другой функции с `@throws`."},
	"overload":   {doc: "Перегрузка: обязательна для каждой функции или метода с тем же именем и другой сигнатурой."},
	"inline":     {doc: "Подсказка компилятору встроить функцию."},
	"noinline":   {doc: "Запрет встраивания функции."},
	"hot":        {doc: "Часто вызываемая функция (оптимизировать агрессивнее)."},
	"cold":       {doc: "Редко вызываемая функция."},
	"nogc":       {doc: "Функция не выделяет память под управлением GC."},
	"packed":     {doc: "Структура без выравнивания полей."},
	"align":      {doc: "Выравнивание структуры: `@align(16)`.", snippet: "align(${1:16})"},
	"export":     {doc: "Экспорт функции в C под заданным именем: `@export(\"name\")`.", snippet: "export(\"${1:name}\")"},
	"target":     {doc: "Сборка только для указанных платформ: `@target(\"linux\")`.", snippet: "target(\"${1:linux}\")"},
	"deprecated": {doc: "Устаревшее объявление (предупреждение при использовании).", snippet: "deprecated(\"${1:use X}\")"},
	"extern":     {doc: "Внешняя функция C без тела: `@extern(\"puts\")`.", snippet: "extern(\"${1:name}\")"},
	"be_to_le":   {doc: "Интринсик: перестановка байтов big-endian → little-endian.", snippet: "be_to_le(${1:x})"},
	"le_to_be":   {doc: "Интринсик: перестановка байтов little-endian → big-endian.", snippet: "le_to_be(${1:x})"},
}

var directiveDocs = map[string]string{
	"gocpy:ignore:unused": "Подавляет ошибку о неиспользуемом объявлении на следующей строке.",
}
