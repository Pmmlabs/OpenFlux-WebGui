# Форк goja

Локальная копия `github.com/dop251/goja v0.0.0-20260917113740-793a2a65c13b`
(подключена через `replace` в `go.mod`) с единственным патчем.

## Патч

`vm.go`, 9 строк (~2525–2677): формат сообщений TypeError при чтении
свойств у undefined/null приведён к виду современного Chrome (93+):

- upstream: `Cannot read property 'x' of undefined`
- форк:     `Cannot read properties of undefined (reading 'x')`
- форк:     `Cannot read properties of null (reading 'x')`

## Зачем

greed.js (антибот-скрипт Яндекса, см. `transport/yandex/greedvm.go`)
специально провоцирует ошибки чтения свойств внутри try/catch и
записывает текст ошибки в поля отпечатка (rdata). UA клиента —
Chrome 141, и сервер может сверять формат сообщений с версией браузера.

Эти ошибки бросаются интерпретатором goja и перехватываются JS-кодом
внутри greed.js — переписать их текст снаружи (из Go или JS) нельзя,
поэтому без этого патча поля отпечатка расходятся с реальным Chrome
и капча эскалирует до силуэтной.

## Обновление

При обновлении goja: скопировать новую версию сюда и повторить замену
строк в `vm.go` (искать `Cannot read property '%s' of undefined`).
Полный diff с upstream — только эти 9 строк.
