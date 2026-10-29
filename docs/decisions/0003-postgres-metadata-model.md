# ADR 0003: Esquema y repositorios iniciales de metadatos

- Estado: aceptada
- Fecha: 2026-10-29

## Decisión

Crear tablas `buckets` y `objects` con IDs `BIGINT IDENTITY`, nombres/keys con
collation `C`, unicidad por nombre de bucket y por `(bucket_id, object_key)`,
FK restrictiva, tamaño no negativo y referencia opaca a bytes externos. ETag se
mantiene opaco; checksum se almacena como algoritmo y valor separados. No se
implementa normalización de nombres S3, versionado, lifecycle, IAM ni borrado de
bytes.

Los repositorios usan inserción create-only para `Put`, actualización explícita
para `Update`, y errores internos mediante sentinels. Los listados emplean
keyset cursors con una ordenación C estable. El borrado de bucket usa una
transacción y bloqueo de fila, con la FK como guardia final contra una inserción
concurrente.

El migrador serializa runners mediante advisory lock transaccional. Cada SQL y
su versión se confirman juntos; el bootstrap de la tabla de versiones es
independiente. Una migración fallida revierte solo su propia transacción; las
versiones previas siguen aplicadas.

## Límites

ETag no implica MD5. El cursor no conserva un snapshot entre llamadas. No existe
atomicidad con el object store: actualizar/eliminar referencias no modifica
bytes. No hay reversión automática ni checksum de migraciones aplicadas.
