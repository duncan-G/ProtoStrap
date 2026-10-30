using System.Buffers;
using System.Globalization;
using System.Text;
using System.Text.Encodings.Web;
using System.Text.Json;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;
using Microsoft.Extensions.Logging.Console;

namespace Sample.Shared;

/// <summary>JsonLineFormatter writes each log entry as one JSON object per line, with the replica's identity.</summary>
public sealed class JsonLineFormatter(Identity identity) : ConsoleFormatter(FormatterName)
{
    public const string FormatterName = "protostrap-json";

    private static readonly JsonWriterOptions Options = new() { Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping };

    public override void Write<TState>(in LogEntry<TState> logEntry, IExternalScopeProvider? scopeProvider, TextWriter textWriter)
    {
        var message = logEntry.Formatter(logEntry.State, logEntry.Exception);
        if (message is null && logEntry.Exception is null)
            return;

        var buffer = new ArrayBufferWriter<byte>(256);
        using (var json = new Utf8JsonWriter(buffer, Options))
        {
            json.WriteStartObject();
            json.WriteString("time", DateTimeOffset.UtcNow);
            json.WriteString("level", logEntry.LogLevel.ToString());
            json.WriteString("msg", message);
            json.WriteString("category", logEntry.Category);
            json.WriteString("component", identity.Component);
            json.WriteString("replica", identity.Replica);
            json.WriteString("release", identity.Release);
            if (logEntry.State is IEnumerable<KeyValuePair<string, object?>> fields)
            {
                foreach (var (key, value) in fields)
                {
                    if (key != "{OriginalFormat}")
                        json.WriteString(key, Convert.ToString(value, CultureInfo.InvariantCulture));
                }
            }
            if (logEntry.Exception is not null)
                json.WriteString("exception", logEntry.Exception.ToString());
            json.WriteEndObject();
        }
        textWriter.Write(Encoding.UTF8.GetString(buffer.WrittenSpan));
        textWriter.Write('\n');
    }
}
