using Npgsql;
using Sample.Migrate;
using Sample.Shared;

// This task brings the schema up to date, then exits 0, or non-zero if it couldn't.
var builder = Host.CreateApplicationBuilder(args);
builder.AddProtoStrap("migrate");
using var host = builder.Build(); // The host is disposed last, which flushes the logs.
var log = host.Services.GetRequiredService<ILoggerFactory>().CreateLogger("Sample.Migrate");

try
{
    await using var db = NpgsqlDataSource.Create(builder.Configuration.RequiredConnectionString("postgres"));
    // PostgreSQL may still be starting, in a new environment.
    await using var connection = await Migrations.OpenAsync(db, TimeSpan.FromMinutes(2), log);
    await Migrations.ApplyAsync(connection, log);
    return 0;
}
catch (Exception ex)
{
    log.LogCritical(ex, "Migrating failed");
    return 1;
}
