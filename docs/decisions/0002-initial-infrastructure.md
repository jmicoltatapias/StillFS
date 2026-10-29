# ADR 0002: Infraestructura inicial con PostgreSQL

- Estado: aceptada
- Fecha: 2026-10-29

## Contexto

La fase inicial necesita un servicio que pueda arrancar, informar su estado y
conectarse a PostgreSQL sin adelantar el modelo definitivo de objetos ni las
operaciones S3.

## Decisiones

- Usar `database/sql` con `github.com/lib/pq` para el pool PostgreSQL. El driver
  y sus componentes MIT/BSD-3-Clause se registran en las notas de licencias; la
  revisión legal completa sigue pendiente.
- Verificar CA y hostname (`verify-full`) por defecto. Compose de desarrollo
  configura explícitamente `disable` para la conexión local de su red privada.
- Ejecutar migraciones SQL numeradas al inicio, una transacción por migración,
  y fallar el arranque si hay error. Crear `stillfs_schema_migrations` como
  infraestructura. Aún no hay migraciones de esquema de aplicación.
- Mantener interfaces separadas para bytes y metadatos; ninguna implementación
  concreta de object store se incluye en esta fase.
- Exponer solo `GET /health/live` y `GET /health/ready` en la API del backend.
  La readiness consulta PostgreSQL con timeout; errores internos no se devuelven
  en el cuerpo HTTP.
- Servir `web/dist` generado por Vite cuando esté presente. No añadir una capa
  de routing ni dependencias de UI adicionales; la navegación inicial vive en
  estado React local.

## Consecuencias y límites

- El Compose es un entorno de desarrollo con volumen PostgreSQL, credenciales
  de conveniencia y TLS desactivado; no es configuración de producción.
- Una migración fallida detiene StillFS. No se define rollback de datos ni
  compatibilidad entre versiones de esquema y binario.
- La revisión de licencias de dependencias y sus obligaciones de distribución
  no está completa.
