using Amazon.SQS;
using Npgsql;
using Sample.Shared;
using Sample.Worker;
using StackExchange.Redis;

var builder = Host.CreateApplicationBuilder(args);
builder.AddProtoStrap("jobs");
// A receive already waiting (up to 20 s) and the jobs in flight (up to 10 s each) finish
// inside the spec's 60 s stop.
builder.Services.Configure<HostOptions>(options => options.ShutdownTimeout = TimeSpan.FromSeconds(50));

// The PostgreSQL and Redis connections are made on first use, so that without a queue (a
// local run) the worker idles without them.
builder.Services.AddSingleton(services =>
    NpgsqlDataSource.Create(services.GetRequiredService<IConfiguration>().RequiredConnectionString("postgres")));
builder.Services.AddSingleton<IConnectionMultiplexer>(services =>
{
    var options = StackExchange.Redis.ConfigurationOptions.Parse(
        services.GetRequiredService<IConfiguration>().RequiredConnectionString("redis"));
    options.AbortOnConnectFail = false; // The multiplexer keeps retrying while Redis starts.
    return ConnectionMultiplexer.Connect(options);
});
builder.Services.AddSingleton<IAmazonSQS>(_ => new AmazonSQSClient());
builder.Services.AddHostedService<JobRunner>();

builder.Build().Run();
