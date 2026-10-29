# Especificación técnica de StillFS (fase inicial)

## Requisitos confirmados

Estos puntos vienen de la solicitud de fase inicial y delimitan este incremento:

- Backend en Go con punto de entrada, configuración tipada validada al inicio,
  logging estructurado, manejo explícito de errores y apagado ordenado.
- PostgreSQL como base de datos de metadatos: configuración de conexión, pool,
  comprobación de salud y migraciones versionadas.
- No establecer aún el esquema definitivo de objetos ni implementar operaciones
  S3. No persistir bytes de objetos en PostgreSQL.
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
- PostgreSQL almacena metadatos e infraestructura de migración solamente; los
  bytes de objetos quedan detrás de una interfaz de almacenamiento separada.
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
- El servidor aplica las migraciones numeradas existentes al inicio, en
  transacciones individuales, y registra su versión en
  `stillfs_schema_migrations`; aún no existe migración de esquema de aplicación.
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
- Política de retención, identidad, autenticación, TLS y secretos fuera del
  entorno de desarrollo.
- Revisión legal completa de dependencias y distribución del texto legal de
  AGPL-3.0. Esta especificación no afirma que esa revisión haya ocurrido.

## Fuera de alcance

Operaciones S3, autenticación de usuarios, administración, clustering,
replicación y garantías de disponibilidad distribuida.
