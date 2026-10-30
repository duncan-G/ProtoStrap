using System.Globalization;

namespace Sample.Shared;

/// <summary>Identity says who this process is, from the variables ProtoStrap sets in every replica.</summary>
public sealed record Identity(string App, string Component, string Replica, string Release)
{
    /// <param name="component">The component's name is used when PROTOSTRAP_COMPONENT isn't set (local runs).</param>
    public static Identity FromEnvironment(string component) => new(
        Env.Get("PROTOSTRAP_APP") ?? "sample",
        Env.Get("PROTOSTRAP_COMPONENT") ?? component,
        Env.Get("PROTOSTRAP_REPLICA") ?? Environment.MachineName,
        Env.Get("PROTOSTRAP_RELEASE") ?? "local");
}

public static class Env
{
    /// <summary>Get returns the variable's value, or null when it is unset or empty.</summary>
    public static string? Get(string name) =>
        Environment.GetEnvironmentVariable(name) is { Length: > 0 } value ? value : null;

    public static int Int(string name, int fallback) =>
        int.TryParse(Get(name), CultureInfo.InvariantCulture, out var value) ? value : fallback;
}
