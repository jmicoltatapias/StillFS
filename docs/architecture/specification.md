# Especificación técnica de StillFS (fase inicial)

## Requisitos confirmados

Estos puntos vienen de la solicitud de fase inicial y delimitan este incremento:

- Backend en Go con punto de entrada, configuración tipada validada al inicio,
  logging estructurado, manejo explícito de errores y apagado ordenado.
- PostgreSQL como base de datos de metadatos: configuración de conexión, pool,
  comprobación de salud y migraciones versionadas.
- PostgreSQL persiste metadatos e identidad mínima; los bytes de objetos siguen
  fuera de PostgreSQL.
- Definir interfaces internas distintas para el almacenamiento de bytes de
  objetos y para la persistencia de metadatos, sin fingir implementaciones.
- Frontend React con TypeScript, organización modular, navegación básica y una
  página que presente el estado real del sistema. No mostrar controles de
  administración para capacidades inexistentes.
- Compose de desarrollo con PostgreSQL y StillFS, volumen persistente,
  comprobaciones de salud y configuración por variables de entorno.
- Makefile, pruebas unitarias e integración básicas, gofmt, go vet, compilación
  del frontend y workflows de CI.
- Documentar instalación, configuración, pruebas, arranque y migraciones.
- Preparar el proyecto para AGPL-3.0 sin declarar concluida una revisión legal
  de dependencias.

## Decisiones de esta fase

- Se conserva la compatibilidad con la versión Go declarada por el módulo hasta
  que una dependencia revisada requiera justificar un cambio.
- PostgreSQL almacena metadatos, principals y secretos de credenciales cifrados;
  los bytes de objetos quedan detrás de una interfaz separada.
- El esquema inicial incluye buckets y metadatos de objetos, con IDs internos
  `BIGINT IDENTITY`, unicidad de nombre y de `(bucket, key)`, referencia opaca
  de contenido externo, tamaño no negativo y `ETag` separado del checksum.
- Los repositorios usan cursores keyset ordenados por nombre/key C-collated. La
  paginación evita desplazamientos de offset, pero no conserva snapshot entre
  llamadas.
- El driver inicial es `github.com/lib/pq` v1.10.9 sobre `database/sql`; su
  upstream declara MIT y contiene porciones SCRAM con licencia BSD-3-Clause.
- El modo TLS PostgreSQL por defecto es `verify-full`; Compose lo establece
  explícitamente como `disable` para su red local de desarrollo.
- La única pantalla del frontend informa estado del servicio y de PostgreSQL
  mediante datos suministrados por endpoints de salud documentados.
- `GET /health/live` responde 200 mientras el proceso HTTP atiende; `GET
  /health/ready` responde 200 con `{"status":"ok","database":"ok"}` si la
  base responde, o 503 con `{"status":"not_ready","database":"unavailable"}`.
- El servidor aplica migraciones numeradas al inicio en transacciones
  individuales. `0001` crea buckets/objetos, `0002` añade principals,
  credenciales cifradas y ownership nullable de buckets; `0003` añade permisos
  por clave/auditoría, y `0004` añade verificación de la clave maestra.
- La tarea 004 añade un subconjunto path-style de S3 API v1, autenticado con
  SigV4 de cabecera, scope de región `s3` y payload SHA-256 firmado. No se
  declara compatibilidad general con S3.
- Los buckets se asignan a principals. Los buckets anteriores a la migración
  `0002` permanecen sin owner y no se exponen por S3 hasta una asignación
  explícita.
- La tarea 006 añade permisos fijos por credencial S3 con denegación por
  defecto y auditoría PostgreSQL de cambios de credenciales/permisos. No añade
  IAM de AWS ni una API administrativa HTTP; la gestión sigue siendo local
  mediante `stillfsctl`.
- La UI de estado vive bajo `/console/`; `/` queda reservado para listar
  buckets autenticados.
- La UI se sirve desde `web/dist` cuando existe el artefacto generado; Vite usa
  un proxy local hacia el backend durante desarrollo frontend.
- El despliegue Compose es para desarrollo local, no una afirmación de
  preparación para producción.

## Decisiones pendientes

- Cómo se operarán/revertirán cambios de migración una vez exista un esquema de
  datos real; esta fase revierte solo la transacción fallida, conserva las
  migraciones previas y detiene el arranque. No implementa downgrade ni
  checksum de archivos ya aplicados.
- Protocolo de coordinación entre los bytes externos y los metadatos cuando
  una operación de almacenamiento falle entre ambos sistemas.
- Política de reconciliación de huérfanos y referencias obsoletas entre
  PostgreSQL y el backend local; el almacenamiento local actual solo compensa
  fallos detectados durante la operación y elimina temporales reservados al
  iniciar.
- Uso multiproceso de una raíz local, garantías específicas por filesystem y
  pruebas de caída abrupta.
- IAM/policies completos, TLS integrado, controles
  anti-replay adicionales y políticas de secretos fuera del entorno de
  desarrollo.
- Pruebas de interoperabilidad con AWS CLI y otros SDKs; la tarea 005 ejecutó
  la suite documentada con AWS SDK for Go v2 `service/s3` v1.113.2, pero no
  valida AWS CLI ni otras versiones/clientes.
- Revisión legal completa de dependencias y distribución del texto legal de
  AGPL-3.0. Esta especificación no afirma que esa revisión haya ocurrido.

## Fuera de alcance

Multipart, versionado S3, IAM completo, lifecycle, federación, administración
web, clustering, replicación y garantías de disponibilidad distribuida.
