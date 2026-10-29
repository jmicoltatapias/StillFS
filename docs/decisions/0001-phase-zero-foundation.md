# ADR 0001: Fase 0, base del servidor

- Estado: aceptada
- Fecha: 2026-10-29

## Contexto

StillFS necesita una base ejecutable antes de incorporar operaciones S3 o persistencia. La primera fase debe validar configuración al inicio, aceptar señales de terminación y mantener pequeñas las dependencias.

## Decisión

El ejecutable vive en `cmd/stillfs`. La configuración de proceso se valida en `internal/config` y el logging estructurado JSON Lines usa la biblioteca estándar para conservar compatibilidad con Go 1.18. El servidor usa `net/http` y cierra de forma ordenada ante cancelación.

El handler inicial responde 404 a todas las rutas. No expone endpoints de S3, no almacena objetos ni metadatos y no incorpora comunicación entre nodos. Las futuras rutas HTTP deberán delegar en lógica de dominio; las interfaces de datos y metadatos se definirán cuando exista una implementación concreta.

## Consecuencias

- La ejecución y la validación de configuración pueden probarse sin servicios externos.
- No se declara compatibilidad S3 ni persistencia en esta fase.
- El formato de logging evita registrar datos de solicitudes por defecto; los llamadores deben excluir secretos de cualquier campo explícito.
- El listener por defecto usa `:9000`, configurable mediante `STILLFS_HTTP_ADDR`.
