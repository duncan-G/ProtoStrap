using System.Text.Json;
using Amazon.SecretsManager;
using Amazon.SecretsManager.Model;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Console;

namespace Sample.Shared;

public static class ProtoStrapHosting
{
    /// <summary>
    /// AddProtoStrap sets up what every .NET component does the same way: its identity, JSON
    /// logs on stdout, and its secret merged into configuration.
    /// </summary>
    public static Identity AddProtoStrap(this IHostApplicationBuilder builder, string component)
    {
        var identity = Identity.FromEnvironment(component);
        builder.Services.AddSingleton(identity);

        builder.Logging.ClearProviders();
        builder.Logging.AddConsole(options => options.FormatterName = JsonLineFormatter.FormatterName);
        builder.Logging.AddConsoleFormatter<JsonLineFormatter, ConsoleFormatterOptions>();
        // A line per request would cost more than the requests under load.
        builder.Logging.AddFilter("Microsoft.AspNetCore", LogLevel.Warning);

        builder.Configuration.AddSecret(Env.Get("SECRET_ID"), Env.Get("SECRET_PREFIX"));
        return identity;
    }

    /// <summary>
    /// AddSecret adds the component's keys from the environment's secret, a JSON object of
    /// key to string: those that start with its prefix, SECRET_PREFIX, and the Shared_ ones,
    /// without their prefixes, its own winning. Keys are in environment-variable form
    /// (Api_ConnectionStrings__postgres), so that a local run can set the same keys, without
    /// their prefixes, as environment variables instead. Without a secret id, nothing is added.
    /// </summary>
    public static IConfigurationBuilder AddSecret(this IConfigurationBuilder configuration, string? secretId, string? prefix)
    {
        if (secretId is null)
            return configuration;

        // The default chains find the agent's container credentials and AWS_REGION.
        using var client = new AmazonSecretsManagerClient();
        var secret = client.GetSecretValueAsync(new GetSecretValueRequest { SecretId = secretId })
            .GetAwaiter().GetResult();
        var values = JsonSerializer.Deserialize<Dictionary<string, string>>(secret.SecretString)
            ?? throw new InvalidOperationException($"Secret {secretId} isn't a JSON object");

        // Without a prefix, from an agent older than the environment's secret, the secret is
        // the component's own, and every key in it is.
        var keys = prefix is null ? values : new Dictionary<string, string>();
        if (prefix is not null)
        {
            foreach (var (key, value) in values)
                if (key.StartsWith("Shared_", StringComparison.Ordinal))
                    keys[key["Shared_".Length..]] = value;
            foreach (var (key, value) in values)
                if (key.StartsWith(prefix, StringComparison.Ordinal))
                    keys[key[prefix.Length..]] = value;
        }

        return configuration.AddInMemoryCollection(keys.Select(pair => KeyValuePair.Create(
            pair.Key.Replace("__", ConfigurationPath.KeyDelimiter, StringComparison.Ordinal), (string?)pair.Value)));
    }

    public static string RequiredConnectionString(this IConfiguration configuration, string name) =>
        configuration.GetConnectionString(name) is { Length: > 0 } value
            ? value
            : throw new InvalidOperationException(
                $"ConnectionStrings__{name} is not set: it comes from the environment's secret, or from the environment in a local run");
}
