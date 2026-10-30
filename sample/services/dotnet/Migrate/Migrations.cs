using System.Net.Sockets;
using Npgsql;

namespace Sample.Migrate;

/// <summary>
/// Migrations holds the schema as numbered steps, each applied once and in order, recorded
/// in schema_migrations, under an advisory lock so two runs never interleave.
/// Keep every step compatible with the previous release, which may run against the new
/// schema after a rollback: add tables and columns, don't rename or drop them.
/// </summary>
public static class Migrations
{
    private const long LockKey = 7_340_001; // Any number works if every run uses the same one.

    private static readonly (int Version, string Sql)[] Steps =
    [
        (1, """
            CREATE TABLE IF NOT EXISTS jobs_done (
                id      text PRIMARY KEY,
                done_at timestamptz NOT NULL DEFAULT now(),
                replica text
            )
            """),
        (2, """
            CREATE TABLE IF NOT EXISTS hits (
                id        bigserial PRIMARY KEY,
                component text,
                at        timestamptz DEFAULT now()
            )
            """),
    ];

    public static async Task<NpgsqlConnection> OpenAsync(NpgsqlDataSource db, TimeSpan patience, ILogger log)
    {
        var deadline = DateTime.UtcNow + patience;
        while (true)
        {
            try
            {
                return await db.OpenConnectionAsync();
            }
            catch (Exception ex) when ((ex is NpgsqlException or SocketException) && DateTime.UtcNow < deadline)
            {
                log.LogWarning("PostgreSQL isn't reachable yet, trying again: {Error}", ex.Message);
                await Task.Delay(TimeSpan.FromSeconds(3));
            }
        }
    }

    public static async Task ApplyAsync(NpgsqlConnection connection, ILogger log)
    {
        await ExecuteAsync(connection, null, "SELECT pg_advisory_lock(@p)", LockKey);
        try
        {
            await ExecuteAsync(connection, null, """
                CREATE TABLE IF NOT EXISTS schema_migrations (
                    version    int PRIMARY KEY,
                    applied_at timestamptz NOT NULL DEFAULT now()
                )
                """);

            var applied = new HashSet<int>();
            await using (var command = new NpgsqlCommand("SELECT version FROM schema_migrations", connection))
            await using (var reader = await command.ExecuteReaderAsync())
            {
                while (await reader.ReadAsync())
                    applied.Add(reader.GetInt32(0));
            }

            foreach (var (version, sql) in Steps.Where(step => !applied.Contains(step.Version)))
            {
                await using var transaction = await connection.BeginTransactionAsync();
                await ExecuteAsync(connection, transaction, sql);
                await ExecuteAsync(connection, transaction, "INSERT INTO schema_migrations (version) VALUES (@p)", version);
                await transaction.CommitAsync();
                log.LogInformation("Applied migration {Version}", version);
            }
            log.LogInformation("Schema is at version {Version}", Steps[^1].Version);
        }
        finally
        {
            await ExecuteAsync(connection, null, "SELECT pg_advisory_unlock(@p)", LockKey);
        }
    }

    private static async Task ExecuteAsync(NpgsqlConnection connection, NpgsqlTransaction? transaction, string sql, object? parameter = null)
    {
        await using var command = new NpgsqlCommand(sql, connection, transaction);
        if (parameter is not null)
            command.Parameters.AddWithValue("p", parameter);
        await command.ExecuteNonQueryAsync();
    }
}
