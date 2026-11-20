<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration {
    public function up(): void
    {
        Schema::create('v2_server_report_batches', function (Blueprint $table) {
            $table->bigIncrements('id');
            $table->unsignedBigInteger('server_id');
            $table->string('server_type', 32);
            $table->string('protocol', 32);
            $table->char('report_id', 32);
            $table->char('payload_hash', 64);
            $table->longText('payload_json');
            $table->longText('server_snapshot_json');
            $table->string('status', 16)->default('pending');
            $table->unsignedBigInteger('record_at');
            $table->unsignedBigInteger('applied_at')->nullable();
            $table->unsignedBigInteger('created_at');
            $table->unsignedBigInteger('updated_at');
            $table->unique(['server_id', 'server_type', 'report_id'], 'server_report_dedupe');
            $table->index(['status', 'id'], 'server_report_pending');
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('v2_server_report_batches');
    }
};
